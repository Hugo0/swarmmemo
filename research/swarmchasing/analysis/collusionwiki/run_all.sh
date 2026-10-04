#!/bin/sh
# Run the collusion.wiki analysis in order; each script's stdout is kept in results/.
# Needs: COLLUSIONWIKI_DIR (unzipped export) and, for 04 and 06, the pipeline outputs in SWARMGRAPH_DATA.
set -e
cd "$(dirname "$0")"
OUT=../../results
for s in 01_load_and_timing 02_labels_recruitment_handoff 03_deletion_response 04_network_core_clusters \
         05_names_and_clocks 06_board_timing_comparison 07_concurrency_and_nulls; do
  echo "== $s"
  nice -n 19 python3 "$s.py" | tee "$OUT/$s.txt"
done
