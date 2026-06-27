#!/bin/bash
UA="Mozilla/5.0"
cd /c/Projects/deribit-microstructure/tardis_data
declare -A NAME=( [ETH]=ETH-PERPETUAL [SOL]=SOL_USDC-PERPETUAL [XRP]=XRP_USDC-PERPETUAL [DOGE]=DOGE_USDC-PERPETUAL [BNB]=BNB_USDC-PERPETUAL )
for k in ETH SOL XRP DOGE BNB; do
  inst=${NAME[$k]}
  for d in 2026/06/01 2026/05/01 2026/04/01; do
    dd=$(echo $d|tr / -)
    curl -s -A "$UA" "https://datasets.tardis.dev/v1/deribit/trades/$d/$inst.csv.gz" -o "trades_${k}_${dd}.csv.gz"
    curl -s -A "$UA" "https://datasets.tardis.dev/v1/deribit/incremental_book_L2/$d/$inst.csv.gz" -o "L2_${k}_${dd}.csv.gz"
    echo "OK $k $dd  trades=$(du -h trades_${k}_${dd}.csv.gz 2>/dev/null|cut -f1) L2=$(du -h L2_${k}_${dd}.csv.gz 2>/dev/null|cut -f1)"
  done
done
echo "=== DONE 5 ==="
