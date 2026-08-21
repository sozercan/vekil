#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  printf 'usage: %s <zen.mdx|->\n' "$0" >&2
  exit 2
fi

source_path="$1"
if [[ "${source_path}" != "-" && ! -f "${source_path}" ]]; then
  printf 'error: OpenCode Zen documentation not found: %s\n' "${source_path}" >&2
  exit 1
fi

# OpenCode's /zen/v1/models endpoint exposes model IDs but not prices. Join the
# published endpoint and pricing tables by their display label so aliases such
# as Ox Alpha Free -> x-preview-f-free and free models without a -free ID remain
# discoverable without guessing from model names.
awk '
function trim(value) {
  sub(/^[[:space:]]+/, "", value)
  sub(/[[:space:]]+$/, "", value)
  return value
}

function unquote(value) {
  value = trim(value)
  gsub(/`/, "", value)
  return value
}

function fail(message) {
  print "error: " message > "/dev/stderr"
  failed = 1
}

/^##[[:space:]]+Endpoints[[:space:]]*$/ {
  section = "endpoints"
  next
}

/^##[[:space:]]+Pricing[[:space:]]*$/ {
  section = "pricing"
  next
}

/^##+[[:space:]]+/ {
  section = ""
  next
}

section == "endpoints" && /^\|/ {
  split($0, columns, /[|]/)
  label = trim(columns[2])
  model_id = unquote(columns[3])
  endpoint_url = unquote(columns[4])
  if (label == "Model" || label ~ /^-+$/ || model_id == "") {
    next
  }
  if (label in endpoint_id) {
    fail("duplicate endpoint label: " label)
    next
  }
  endpoint_order[++endpoint_count] = label
  endpoint_id[label] = model_id
  endpoint_url_by_label[label] = endpoint_url
  next
}

section == "pricing" && /^\|/ {
  split($0, columns, /[|]/)
  label = trim(columns[2])
  input_price = trim(columns[3])
  output_price = trim(columns[4])
  if (label == "Model" || label ~ /^-+$/) {
    next
  }
  if (label in pricing_label) {
    fail("duplicate pricing label: " label)
    next
  }
  pricing_label[label] = 1
  if (input_price == "Free" && output_price == "Free") {
    free_label[label] = 1
    free_order[++free_count] = label
  }
  next
}

END {
  if (failed) {
    exit 1
  }
  if (free_count == 0) {
    fail("no models with Free input and output labels were found")
    exit 1
  }

  for (i = 1; i <= free_count; i++) {
    label = free_order[i]
    if (!(label in endpoint_id)) {
      fail("free pricing label has no endpoint-table entry: " label)
    }
  }
  if (failed) {
    exit 1
  }

  emitted = 0
  for (i = 1; i <= endpoint_count; i++) {
    label = endpoint_order[i]
    if (!(label in free_label)) {
      continue
    }

    model_id = endpoint_id[label]
    if (length(model_id) > 128 || model_id !~ /^[A-Za-z0-9][A-Za-z0-9._\/-]*$/) {
      fail("invalid free model ID for " label ": " model_id)
      continue
    }

    endpoint = endpoint_url_by_label[label]
    sub(/^https:\/\/opencode\.ai\/zen\/v1/, "", endpoint)
    if (endpoint != "/chat/completions" && endpoint != "/responses" && endpoint != "/messages") {
      fail("unsupported endpoint for " label ": " endpoint_url_by_label[label])
      continue
    }

    print model_id "\t" label "\t" endpoint
    emitted++
  }

  if (failed) {
    exit 1
  }
  if (emitted == 0) {
    fail("no free endpoint rows were emitted")
    exit 1
  }
}
' "$([[ "${source_path}" == "-" ]] && printf '/dev/stdin' || printf '%s' "${source_path}")"
