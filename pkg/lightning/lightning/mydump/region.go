// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package mydump

import (
	"context"
	"math"
	"sync"
	"time"

	"github.com/pingcap/br/pkg/utils"

	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/errors"
	"go.uber.org/zap"

	"github.com/pingcap/tidb-lightning/lightning/config"
	"github.com/pingcap/tidb-lightning/lightning/log"
	"github.com/pingcap/tidb-lightning/lightning/worker"
)

const tableRegionSizeWarningThreshold int64 = 1024 * 1024 * 1024

type TableRegion struct {
	EngineID int32

	DB       string
	Table    string
	FileMeta SourceFileMeta

	Chunk Chunk
}

func (reg *TableRegion) RowIDMin() int64 {
	return reg.Chunk.PrevRowIDMax + 1
}
func (reg *TableRegion) Rows() int64 {
	return reg.Chunk.RowIDMax - reg.Chunk.PrevRowIDMax
}
func (reg *TableRegion) Offset() int64 {
	return reg.Chunk.Offset
}
func (reg *TableRegion) Size() int64 {
	return reg.Chunk.EndOffset - reg.Chunk.Offset
}

////////////////////////////////////////////////////////////////

func AllocateEngineIDs(
	filesRegions []*TableRegion,
	dataFileSizes []float64,
	batchSize float64,
	batchImportRatio float64,
	tableConcurrency float64,
) {
	totalDataFileSize := 0.0
	for _, dataFileSize := range dataFileSizes {
		totalDataFileSize += dataFileSize
	}

	// No need to batch if the size is too small :)
	if totalDataFileSize <= batchSize {
		return
	}

	curEngineID := int32(0)
	curEngineSize := 0.0
	curBatchSize := batchSize

	// import() step will not be concurrent.
	// If multiple Batch end times are close, it will result in multiple
	// Batch import serials. We need use a non-uniform batch size to create a pipeline effect.
	// Here we calculate the total number of engines, which is needed to compute the scale up
	//
	//     Total/B1 = 1/(1-R) * (N - 1/beta(N, R))
	//              ≲ N/(1-R)
	//
	// We use a simple brute force search since the search space is extremely small.
	ratio := totalDataFileSize * (1 - batchImportRatio) / batchSize
	n := math.Ceil(ratio)
	logGammaNPlusR, _ := math.Lgamma(n + batchImportRatio)
	logGammaN, _ := math.Lgamma(n)
	logGammaR, _ := math.Lgamma(batchImportRatio)
	invBetaNR := math.Exp(logGammaNPlusR - logGammaN - logGammaR) // 1/B(N, R) = Γ(N+R)/Γ(N)Γ(R)
	for {
		if n <= 0 || n > tableConcurrency {
			n = tableConcurrency
			break
		}
		realRatio := n - invBetaNR
		if realRatio >= ratio {
			// we don't have enough engines. reduce the batch size to keep the pipeline smooth.
			curBatchSize = totalDataFileSize * (1 - batchImportRatio) / realRatio
			break
		}
		invBetaNR *= 1 + batchImportRatio/n // Γ(X+1) = X * Γ(X)
		n += 1.0
	}

	for i, dataFileSize := range dataFileSizes {
		filesRegions[i].EngineID = curEngineID
		curEngineSize += dataFileSize

		if curEngineSize >= curBatchSize {
			curEngineSize = 0
			curEngineID++

			i := float64(curEngineID)
			// calculate the non-uniform batch size
			if i >= n {
				curBatchSize = batchSize
			} else {
				// B_(i+1) = B_i * (I/W/(N-i) + 1)
				curBatchSize *= batchImportRatio/(n-i) + 1.0
			}
		}
	}
}

func MakeTableRegions(
	ctx context.Context,
	meta *MDTableMeta,
	columns int,
	cfg *config.Config,
	ioWorkers *worker.Pool,
	store storage.ExternalStorage,
) ([]*TableRegion, error) {
	// Split files into regions
	type fileRegionRes struct {
		info    FileInfo
		regions []*TableRegion
		sizes   []float64
		err     error
	}

	start := time.Now()

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	concurrency := utils.MaxInt(cfg.App.RegionConcurrency, 2)
	fileChan := make(chan FileInfo, concurrency)
	resultChan := make(chan fileRegionRes, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			for info := range fileChan {
				regions, sizes, err := makeSourceFileRegion(execCtx, meta, info, columns, cfg, ioWorkers, store)
				select {
				case resultChan <- fileRegionRes{info: info, regions: regions, sizes: sizes, err: err}:
				case <-ctx.Done():
					break
				}
				if err != nil {
					break
				}
			}
			wg.Done()
		}()
	}

	go func() {
		wg.Wait()
		close(resultChan)
	}()

	errChan := make(chan error, 1)
	fileRegionsMap := make(map[string]fileRegionRes, len(meta.DataFiles))
	go func() {
		for res := range resultChan {
			if res.err != nil {
				errChan <- res.err
				return
			}
			fileRegionsMap[res.info.FileMeta.Path] = res
		}
		errChan <- nil
	}()

	for _, dataFile := range meta.DataFiles {
		select {
		case fileChan <- dataFile:
		case <-ctx.Done():
			close(fileChan)
			return nil, ctx.Err()
		case err := <-errChan:
			return nil, err
		}
	}
	close(fileChan)
	err := <-errChan
	if err != nil {
		return nil, err
	}

	filesRegions := make([]*TableRegion, 0, len(meta.DataFiles))
	dataFileSizes := make([]float64, 0, len(meta.DataFiles))
	prevRowIDMax := int64(0)
	for _, dataFile := range meta.DataFiles {
		fileRegionsRes := fileRegionsMap[dataFile.FileMeta.Path]
		var delta int64
		if len(fileRegionsRes.regions) > 0 {
			delta = prevRowIDMax - fileRegionsRes.regions[0].Chunk.PrevRowIDMax
		}

		for _, region := range fileRegionsRes.regions {
			region.Chunk.PrevRowIDMax += delta
			region.Chunk.RowIDMax += delta
		}
		filesRegions = append(filesRegions, fileRegionsRes.regions...)
		dataFileSizes = append(dataFileSizes, fileRegionsRes.sizes...)
		prevRowIDMax = fileRegionsRes.regions[len(fileRegionsRes.regions)-1].Chunk.RowIDMax
	}

	log.L().Info("makeTableRegions", zap.Int("filesCount", len(meta.DataFiles)),
		zap.Int64("maxRegionSize", int64(cfg.Mydumper.MaxRegionSize)),
		zap.Int("RegionsCount", len(filesRegions)),
		zap.Duration("cost", time.Since(start)))

	AllocateEngineIDs(filesRegions, dataFileSizes, float64(cfg.Mydumper.BatchSize), cfg.Mydumper.BatchImportRatio, float64(cfg.App.TableConcurrency))
	return filesRegions, nil
}

func makeSourceFileRegion(
	ctx context.Context,
	meta *MDTableMeta,
	fi FileInfo,
	columns int,
	cfg *config.Config,
	ioWorkers *worker.Pool,
	store storage.ExternalStorage,
) ([]*TableRegion, []float64, error) {
	if fi.FileMeta.Type == SourceTypeParquet {
		_, region, err := makeParquetFileRegion(ctx, store, meta, fi, 0)
		if err != nil {
			return nil, nil, err
		}
		return []*TableRegion{region}, []float64{float64(fi.FileMeta.FileSize)}, nil
	}

	dataFileSize := fi.FileMeta.FileSize
	divisor := int64(columns)
	isCsvFile := fi.FileMeta.Type == SourceTypeCSV
	if !isCsvFile {
		divisor += 2
	}
	// If a csv file is overlarge, we need to split it into multiple regions.
	// Note: We can only split a csv file whose format is strict.
	if isCsvFile && dataFileSize > int64(cfg.Mydumper.MaxRegionSize) && cfg.Mydumper.StrictFormat {

		_, regions, subFileSizes, err := SplitLargeFile(ctx, meta, cfg, fi, divisor, 0, ioWorkers, store)
		return regions, subFileSizes, err
	}

	tableRegion := &TableRegion{
		DB:       meta.DB,
		Table:    meta.Name,
		FileMeta: fi.FileMeta,
		Chunk: Chunk{
			Offset:       0,
			EndOffset:    fi.FileMeta.FileSize,
			PrevRowIDMax: 0,
			RowIDMax:     fi.FileMeta.FileSize / divisor,
		},
	}

	if tableRegion.Size() > tableRegionSizeWarningThreshold {
		log.L().Warn(
			"file is too big to be processed efficiently; we suggest splitting it at 256 MB each",
			zap.String("file", fi.FileMeta.Path),
			zap.Int64("size", dataFileSize))
	}
	return []*TableRegion{tableRegion}, []float64{float64(fi.FileMeta.FileSize)}, nil
}

// because parquet files can't seek efficiently, there is no benefit in split.
// parquet file are column orient, so the offset is read line number
func makeParquetFileRegion(
	ctx context.Context,
	store storage.ExternalStorage,
	meta *MDTableMeta,
	dataFile FileInfo,
	prevRowIdxMax int64,
) (int64, *TableRegion, error) {
	r, err := store.Open(ctx, dataFile.FileMeta.Path)
	if err != nil {
		return prevRowIdxMax, nil, errors.Trace(err)
	}
	numberRows, err := ReadParquetFileRowCount(ctx, store, r, dataFile.FileMeta.Path)
	rowIDMax := prevRowIdxMax + numberRows
	region := &TableRegion{
		DB:       meta.DB,
		Table:    meta.Name,
		FileMeta: dataFile.FileMeta,
		Chunk: Chunk{
			Offset:       0,
			EndOffset:    numberRows,
			PrevRowIDMax: prevRowIdxMax,
			RowIDMax:     rowIDMax,
		},
	}
	return rowIDMax, region, nil
}

// SplitLargeFile splits a large csv file into multiple regions, the size of
// each regions is specified by `config.MaxRegionSize`.
// Note: We split the file coarsely, thus the format of csv file is needed to be
// strict.
// e.g.
// - CSV file with header is invalid
// - a complete tuple split into multiple lines is invalid
func SplitLargeFile(
	ctx context.Context,
	meta *MDTableMeta,
	cfg *config.Config,
	dataFile FileInfo,
	divisor int64,
	prevRowIdxMax int64,
	ioWorker *worker.Pool,
	store storage.ExternalStorage,
) (prevRowIdMax int64, regions []*TableRegion, dataFileSizes []float64, err error) {
	maxRegionSize := int64(cfg.Mydumper.MaxRegionSize)
	dataFileSizes = make([]float64, 0, dataFile.FileMeta.FileSize/maxRegionSize+1)
	startOffset, endOffset := int64(0), maxRegionSize
	var columns []string
	if cfg.Mydumper.CSV.Header {
		r, err := store.Open(ctx, dataFile.FileMeta.Path)
		if err != nil {
			return 0, nil, nil, err
		}
		parser := NewCSVParser(&cfg.Mydumper.CSV, r, int64(cfg.Mydumper.ReadBlockSize), ioWorker, true)
		if err = parser.ReadColumns(); err != nil {
			return 0, nil, nil, err
		}
		columns = parser.Columns()
		startOffset, _ = parser.Pos()
		endOffset = startOffset + maxRegionSize
	}
	for {
		curRowsCnt := (endOffset - startOffset) / divisor
		rowIDMax := prevRowIdxMax + curRowsCnt
		if endOffset != dataFile.FileMeta.FileSize {
			r, err := store.Open(ctx, dataFile.FileMeta.Path)
			if err != nil {
				return 0, nil, nil, err
			}
			parser := NewCSVParser(&cfg.Mydumper.CSV, r, int64(cfg.Mydumper.ReadBlockSize), ioWorker, false)
			if err = parser.SetPos(endOffset, prevRowIdMax); err != nil {
				return 0, nil, nil, err
			}
			pos, err := parser.ReadUntilTokNewLine()
			if err != nil {
				return 0, nil, nil, err
			}
			endOffset = pos
			parser.Close()
		}
		regions = append(regions,
			&TableRegion{
				DB:       meta.DB,
				Table:    meta.Name,
				FileMeta: dataFile.FileMeta,
				Chunk: Chunk{
					Offset:       startOffset,
					EndOffset:    endOffset,
					PrevRowIDMax: prevRowIdxMax,
					RowIDMax:     rowIDMax,
					Columns:      columns,
				},
			})
		dataFileSizes = append(dataFileSizes, float64(endOffset-startOffset))
		prevRowIdxMax = rowIDMax
		if endOffset == dataFile.FileMeta.FileSize {
			break
		}
		startOffset = endOffset
		if endOffset += maxRegionSize; endOffset > dataFile.FileMeta.FileSize {
			endOffset = dataFile.FileMeta.FileSize
		}
	}
	return prevRowIdxMax, regions, dataFileSizes, nil
}