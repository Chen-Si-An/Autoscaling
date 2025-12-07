#!/bin/bash

mkdir -p logs

i=1
threads=${1:-10}
while true; do
    echo "Starting YCSB run #$i"
    bin/ycsb.sh run mongodb -s   -P workloads/workloadr \
    -p mongodb.url="mongodb://root:mongodb123@my-mongodb-sharded:27017/ycsb?authSource=admin&readPreference=nearest" \
    -threads $threads 2>&1 | tee logs/run_${threads}_${i}.log
    i=$((i+1))
done