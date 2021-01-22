#!/bin/bash
#
# Copyright 2020 PingCAP, Inc.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# See the License for the specific language governing permissions and
# limitations under the License.

# This test is used to test compatible for BR restore.
# It downloads backup data that generated from v4.0.5 to v4.0.10 from fileserver.
# The total kvs is 300000 and size is 203117504. so we only need to check the kvs/size after restore.
set -eux

BUCKET="test"
EXPECTED_KVS=300000

mkdir -p "$DATA_PATH"

# download gcs backup data from file server and extract to storage path.
curl http://lease.pingcap.org/gcs_bk.tar.gz -o $TEST_DIR/gcs_data.tar.gz
tar -zxvf $TEST_DIR/gcs_data.tar.gz -C $DATA_PATH

# start oauth server
bin/oauth &
OAUTH_ID=$!

stop_gcs() {
    kill -2 $OAUTH_ID
}
trap stop_gcs EXIT

# we need start a oauth server or gcs client will failed to handle request.
KEY=$(cat <<- EOF
{
  "type": "service_account",
  "private_key": "-----BEGIN PRIVATE KEY-----\nMIIEvgIBADANBgkqhkiG9w0BAQEFAASCBKgwggSkAgEAAoIBAQCT524vzG7uEVtX\nojcHbyQzVwlcaGkg1DWWLT+SufD08UYF0bsfcD0Etrtzo4ggwdxJQy5ygl3TNlcD\nKdelWbVyGfg9/sNB1RDlZYbQb0LVLHKjkVs7JyJsxrLk2e6NqD9ajwTEJUcLAQkj\nxlCcIi51beqrIRlvHjbtGwet/dNnRLSZf+i9SHvB2j64+RVYdnyf/IiLBvYyu7hF\nT6VjlljdbwC4TZ2jpfDL8nHRTiDiV+CX3/iH8MlMEOSM30AO5MPNVCZLlTA9W24a\nKi4NPBBlJLvG2mQELYdbhdM64iMvbPkDRtajJD6ogPB7wUoWbtSke5oOJNyV1HNt\nn91JH/dlAgMBAAECggEAQBwve2GSbfgxD0Xds4e9+dEO2jLZ6uSBS9TWOywFIa9Z\nqlkUUtbMZDgu/buTXJubeFg6EGGo+M4TnmfrNR2zFD/khj7hdS49kinVa5Dmt895\n66Osl3HprpvcXG2IxXd56q+Woc0Ew+TRiOPD+kGowLcB4ubIhw1iQpmWVRlyos6Q\nyvHssolrqOkRK9+1asixgow2Y15HtpXFN3XDIVj3gfdN1Zg80S66bTap1DS+dkJH\nSMgEZRilAjUGzbroqvZCiymlIJP5Jj5L5Wy8Qp/k1ixK10oaPgwvdmwXHX/DZ0vC\nT6XwpIaCYd3/XUWBHvrmQHFucWVPISZRi5WidggzuwKBgQDNHrxKaDrxcrV5Ncgu\npQrtQvTsIUCJGMo5m30X0Ac5CsIssOoQHdtEQW1ehJ8DtJRRb9rdWc4aelXsDUr+\no2m1zyZzM6S7IO2YhGDAo7Uu3fy1r33qYAt6uS/nHaJBpsKcyqqK+0wPDikdPLLx\nBBWZHF6WoswDEUVLQa/hHgpjPwKBgQC4l2/6xShNoobivzk8AE/Acq7PazA8gu4K\nY0UghTBlAst4RvBTURYZ2V3uw0S2FbfwL0/snHhNWZl5XjBX/H9oQmLri5qGOOpf\n9A11p5kd0x1mHDgTm/k7EgoskdXGB5NqXIB7l/3UI8Sk2N1PzHwyJJYfaB+EWTs8\n+LVy99VQWwKBgQCilRwVtiwSOSPSYWi8YCEbEpljmK+4eye/JZmviDpRYk+qcMf1\n4lRr85gm9OO9YiK1sf0+ufH9Vr5IDflFgG1HqFwHsAWANYdd/n9Z8eior1ehAurB\nHUO8EJEBlaGIfA+Bi7pF0w3kWQsJm5USKHSeGbh3ma4vOD8+eWBZBSCirQKBgQCe\n1uEq/sChnXtIXpgXg4Uc6xJ1tZy6VUgUdDulsjZklTUU+KYQa7QC5kKoFCtqK+It\nseiqiDIVDUa9Y0liTQotYwLQAT8kxJEZpF54oZFmUqX3mcy/QvYB2JIcrBkx4I7/\ndT2yHKX1CBpMZ7h41FMCquzrdaO5NTd+Td2FYrGSBQKBgEBnAerHh/NafYlVumlS\nVgouR9IketTegyEyntVyEvENx8OA5ZLMywCIKbPMFZgPR0RgDpyDxKauCU2E09e/\nboN76UOuOg11fknJh7vFbUbzM6BXvXVOTyX9ZtZBQcd5Y3tV+tYD1tHUgurGYWb+\nyHLBMOlXdpn0gZ4rwoIQgzD9\n-----END PRIVATE KEY-----\n",
  "client_email": "test@email.com",
  "token_uri": "http://localhost:5000/oauth/token"
}
EOF)

# save CREDENTIALS to file
echo $KEY > "tests/$TEST_NAME/config.json"

# export test CREDENTIALS for gcs oauth
export GOOGLE_APPLICATION_CREDENTIALS="tests/$TEST_NAME/config.json"

# do not log to terminal
unset BR_LOG_TO_TERM
LOG_PATH=$TEST_DIR/restore.log

# restore backup data from v4.0.5 to v4.0.10
for i in `seq 5 10`
do
    echo "restore v4.0.$i data starts..."
    LOG_PATH=$TEST_DIR/restore.log.v4.0.$i
    run_br restore db --db sbtest -s "gcs://$BUCKET/bkv4.0.$i" --pd $PD_ADDR --gcs.endpoint="http://$GCS_HOST:$GCS_PORT/storage/v1/" --log-file $LOG_PATH
    kvs=$(cat $LOG_PATH | grep summary |  awk -F 'total kv:' '{print $2}' | awk -F '"' '{print $1}' | awk -F ',' '{print $1}' | xargs)
    if [ $kvs -ne $EXPECTED_KVS ]; then
        echo "restore v4.0.$i data failed due to restore data not as expected"
        cat $LOG_PATH
        exit 1
    fi
done