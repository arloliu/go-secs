#!/bin/sh

i=0
while true; do
    i=$(($i+1))
    echo "===== Test round $i ====="

    make clean

    ENV=development go test -timeout 30s -run ^TestConnection.* github.com/arloliu/go-secs/hsmsss -v
    ret=$?

    if [ ! $ret -eq 0 ]; then
        echo "===== Test round  $i failed ====="
        exit 1
    fi

    # sleep 1s
done