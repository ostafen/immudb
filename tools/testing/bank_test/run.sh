#!/bin/bash

# Check if the binary is provided
if [ -z "$1" ]; then
    echo "Usage: $0 <binary_path> <search_string>"
    exit 1
fi

BINARY="$1"
SEARCH_STRING="$2"
BASENAME=$(basename "$BINARY")
COUNT=1

while true; do
    LOG_FILE="${BASENAME}_${COUNT}.log"
    
    echo "Running $BINARY, logging to $LOG_FILE"
    
    NUM_LEDGERS=$((RANDOM % 6 + 5))

    "$BINARY" --num-ledgers $NUM_LEDGERS --duration 10m &> "$LOG_FILE"
    rm -rf /tmp/ledger-*
    
    if grep -q "$SEARCH_STRING" "$LOG_FILE"; then
        echo "String found, keeping log: $LOG_FILE"
    else
        echo "String not found, deleting log: $LOG_FILE"
        rm "$LOG_FILE"
    fi
    
    ((COUNT++))
done
