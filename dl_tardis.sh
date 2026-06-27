#!/bin/bash
UA="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"
cd /c/Projects/deribit-microstructure/tardis_data
# trades (leves) p/ vários meses + L2 (pesado) começando por 1 dia p/ gauge
for d in 2026/06/01 2026/05/01 2026/04/01 2026/03/01; do
  out="trades_$(echo $d|tr / -).csv.gz"
  curl -s -A "$UA" "https://datasets.tardis.dev/v1/deribit/trades/$d/BTC-PERPETUAL.csv.gz" -o "$out"
  echo "OK $out $(du -h "$out" 2>/dev/null | cut -f1)"
done
for d in 2026/06/01 2026/05/01 2026/04/01; do
  out="L2_$(echo $d|tr / -).csv.gz"
  curl -s -A "$UA" "https://datasets.tardis.dev/v1/deribit/incremental_book_L2/$d/BTC-PERPETUAL.csv.gz" -o "$out"
  echo "OK $out $(du -h "$out" 2>/dev/null | cut -f1)"
done
echo "=== DONE ==="; ls -la
