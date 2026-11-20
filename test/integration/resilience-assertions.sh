#!/usr/bin/env bash
# Read-only assertions shared by the harness and tests; no fixture setup here.
records_valid() {
 local base=$ROOT/apps/shop/production files f id
 server_run test -s "$base/current/release.json" || return 1
 files=$(server_run find "$base/releases" -type f -name release.json) || return 1
 [[ -n $files ]] || return 1
 files="$base/current/release.json
$files"
 while IFS= read -r f; do
  server_run cat "$f" | jq -se 'length == 1 and (.[0] | .app == "shop" and .destination == "production" and (.version | type == "string" and length > 0) and (.operation_id | type == "string" and test("^[A-Za-z0-9_-]{1,128}$")))' >/dev/null || return 1
  id=$(server_run cat "$f" | jq -er .operation_id) || return 1
  # Every Release must reference a complete operation record.
  server_run test -s "$base/operations/$id.json" || return 1
 done <<< "$files"
 files=$(server_run find "$base/operations" -type f -name '*.json') || return 1
 [[ -n $files ]] || return 1
 while IFS= read -r f; do
  server_run cat "$f" | jq -se --arg id "$(basename "$f" .json)" 'length == 1 and (.[0] | .id == $id and (.started_at | type == "string" and length > 0) and (.services | type == "object") and ((.state == "running") or (.state == "finished" and (.outcome == "success" or .outcome == "failed" or .outcome == "partial" or .outcome == "unknown"))))' >/dev/null || return 1
 done <<< "$files"
}

recovery_event() {
 jq -sec --arg id "$2" '
  [.[] | select(.event == "recovery")] |
  select(length == 1) | .[0] |
  select(.schema_version == 1 and .backup_id == $id and
   (.backup_id | type == "string" and length > 0) and
   (.recovery_point | type == "string" and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\\.[0-9]+)?Z$")) and
   (.recovery_point | sub("\\.[0-9]+Z$"; "Z") | (try fromdateiso8601 catch false) | type == "number") and
   (.data_restore_seconds | type == "number" and . >= 0) and
   has("full_recovery_seconds") and .full_recovery_seconds == null and
   has("image_source") and .image_source == null and
   has("scheduled_jobs_reinstalled") and .scheduled_jobs_reinstalled == null and
   (has("needs_old_server") | not))' "$1"
}
