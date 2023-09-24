#!/bin/bash

# log rotate
docker exec -it webapp-nginx-1 /bin/bash /var/log/nginx/nginx-logrotate.sh

# /dev/nullをコピーしているが
# rm mysql-slow.log && mysqladmin flush-logs; でも良い
docker exec -it webapp-mysql-1 /bin/bash -c "cp /dev/null /var/log/mysql/mysql-slow.log"

# benchmark
readonly GAS_POST_URL="https://script.google.com/macros/s/AKfycbzAtaKnPpjujtHZYBpKkvgmd72EtHHceC9EvlZWePScjNEQgQB9w64OuifC8T69YAc/exec"
readonly TARGET_URL="http://localhost"

../../benchmarker/bin/benchmarker -u ../../benchmarker/userdata -t ${TARGET_URL} | tee benchresult.json
curl -s -X POST -H "Content-Type: application/json" -d @benchresult.json -L ${GAS_POST_URL} >/dev/null
rm benchresult.json 
