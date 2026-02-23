#!/usr/bin/env bash
set -euo pipefail

issues_dir=".issues"
output_json=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --json)
      output_json=true
      shift
      ;;
    --help|-h)
      echo "Usage: $0 [--json] [issues_dir]"
      echo
      echo "Build the issue dependency graph from frontmatter depends_on."
      echo
      echo "Examples:"
      echo "  $0"
      echo "  $0 --json"
      echo "  $0 .issues"
      exit 0
      ;;
    --*)
      echo "Unknown flag: $1" >&2
      echo "Usage: $0 [--json] [issues_dir]" >&2
      exit 1
      ;;
    *)
      issues_dir="$1"
      shift
      ;;
  esac
done

trim() {
  local value="$1"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

strip_quotes() {
  local value="$1"
  if [[ "${value:0:1}" == "\"" && "${value: -1}" == "\"" ]]; then
    value="${value:1:${#value}-2}"
  elif [[ "${value:0:1}" == "'" && "${value: -1}" == "'" ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s' "$value"
}

json_escape() {
  local value="$1"
  value="${value//\\/\\\\}"
  value="${value//\"/\\\"}"
  value="${value//$'\n'/\\n}"
  value="${value//$'\r'/\\r}"
  value="${value//$'\t'/\\t}"
  printf '%s' "$value"
}

normalize_bool() {
  local value
  value="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  if [[ "$value" == "true" ]]; then
    printf 'true'
  else
    printf 'false'
  fi
}

parse_dep_list() {
  local raw dep
  raw="$(trim "$1")"
  raw="$(strip_quotes "$raw")"
  raw="${raw#[}"
  raw="${raw%]}"
  raw="$(trim "$raw")"

  if [[ -z "$raw" ]]; then
    printf ''
    return
  fi

  local IFS=','
  read -r -a dep_items <<< "$raw"
  local parsed=()
  for dep in "${dep_items[@]}"; do
    dep="$(strip_quotes "$(trim "$dep")")"
    if [[ -n "$dep" ]]; then
      parsed+=("$dep")
    fi
  done

  local joined=""
  for dep in "${parsed[@]}"; do
    if [[ -n "$joined" ]]; then
      joined+=","
    fi
    joined+="$dep"
  done
  printf '%s' "$joined"
}

join_by() {
  local sep="$1"
  shift
  local out=""
  local item
  for item in "$@"; do
    if [[ -n "$out" ]]; then
      out+="$sep"
    fi
    out+="$item"
  done
  printf '%s' "$out"
}

find_issue_index_by_id() {
  local needle="$1"
  local i
  for ((i=0; i<${#issue_ids[@]}; i++)); do
    if [[ "${issue_ids[$i]}" == "$needle" ]]; then
      printf '%s' "$i"
      return 0
    fi
  done
  printf '%s' "-1"
}

if [[ ! -d "$issues_dir" ]]; then
  echo "Issues directory not found: $issues_dir" >&2
  exit 1
fi

shopt -s nullglob
files=("$issues_dir"/*.md)
IFS=$'\n' files=($(printf '%s\n' "${files[@]}" | sort))
unset IFS

if [[ ${#files[@]} -eq 0 ]]; then
  if [[ "$output_json" == true ]]; then
    printf '{\n  "issues": [],\n  "edges": [],\n  "summary": {"total": 0, "completed": 0, "open": 0}\n}\n'
  else
    echo "No issue files found in $issues_dir"
  fi
  exit 0
fi

issue_ids=()
issue_titles=()
issue_completed=()
issue_files=()
issue_deps_csv=()

for file in "${files[@]}"; do
  id=""
  title=""
  completed=""
  depends_on=""
  delimiter_count=0
  in_frontmatter=false

  while IFS= read -r line; do
    if [[ "$line" == "---" ]]; then
      delimiter_count=$((delimiter_count + 1))
      if [[ $delimiter_count -eq 1 ]]; then
        in_frontmatter=true
        continue
      fi
      if [[ $delimiter_count -eq 2 ]]; then
        break
      fi
    fi

    if [[ "$in_frontmatter" == true ]]; then
      case "$line" in
        id:*)
          id="$(trim "${line#id:}")"
          ;;
        title:*)
          title="$(strip_quotes "$(trim "${line#title:}")")"
          ;;
        completed:*)
          completed="$(strip_quotes "$(trim "${line#completed:}")")"
          ;;
        depends_on:*)
          depends_on="$(parse_dep_list "${line#depends_on:}")"
          ;;
      esac
    fi
  done < "$file"

  if [[ -z "$id" ]]; then
    continue
  fi

  issue_ids+=("$id")
  issue_titles+=("${title:-<no title>}")
  issue_completed+=("$(normalize_bool "$completed")")
  issue_files+=("$file")
  issue_deps_csv+=("$depends_on")
done

if [[ ${#issue_ids[@]} -eq 0 ]]; then
  if [[ "$output_json" == true ]]; then
    printf '{\n  "issues": [],\n  "edges": [],\n  "summary": {"total": 0, "completed": 0, "open": 0}\n}\n'
  else
    echo "No valid issue frontmatter found in $issues_dir"
  fi
  exit 0
fi

# Build reverse dependencies (which issues each issue blocks).
issue_blocks_csv=()
for ((i=0; i<${#issue_ids[@]}; i++)); do
  issue_blocks_csv+=("")
done

for ((i=0; i<${#issue_ids[@]}; i++)); do
  deps_csv="${issue_deps_csv[$i]}"
  [[ -z "$deps_csv" ]] && continue

  IFS=',' read -r -a deps <<< "$deps_csv"
  for dep in "${deps[@]}"; do
    dep="$(trim "$dep")"
    [[ -z "$dep" ]] && continue

    dep_index="$(find_issue_index_by_id "$dep")"
    if [[ "$dep_index" == "-1" ]]; then
      continue
    fi

    current="${issue_blocks_csv[$dep_index]}"
    if [[ -n "$current" ]]; then
      current+=","
    fi
    current+="${issue_ids[$i]}"
    issue_blocks_csv[$dep_index]="$current"
  done
done

completed_count=0
for status in "${issue_completed[@]}"; do
  if [[ "$status" == "true" ]]; then
    completed_count=$((completed_count + 1))
  fi
done
total_count="${#issue_ids[@]}"
open_count=$((total_count - completed_count))

if [[ "$output_json" == true ]]; then
  printf '{\n'
  printf '  "issues": [\n'
  for ((i=0; i<${#issue_ids[@]}; i++)); do
    id="${issue_ids[$i]}"
    title="${issue_titles[$i]}"
    completed="${issue_completed[$i]}"
    file="${issue_files[$i]}"
    deps_csv="${issue_deps_csv[$i]}"
    blocks_csv="${issue_blocks_csv[$i]}"

    printf '    {\n'
    printf '      "id": %s,\n' "$(json_escape "$id")"
    printf '      "title": "%s",\n' "$(json_escape "$title")"
    printf '      "completed": %s,\n' "$completed"
    printf '      "file": "%s",\n' "$(json_escape "$file")"

    printf '      "depends_on": ['
    if [[ -n "$deps_csv" ]]; then
      IFS=',' read -r -a deps <<< "$deps_csv"
      for j in "${!deps[@]}"; do
        dep="$(trim "${deps[$j]}")"
        printf '%s' "$(json_escape "$dep")"
        if [[ "$j" -lt $((${#deps[@]} - 1)) ]]; then
          printf ', '
        fi
      done
    fi
    printf '],\n'

    printf '      "blocks": ['
    if [[ -n "$blocks_csv" ]]; then
      IFS=',' read -r -a blocks <<< "$blocks_csv"
      for j in "${!blocks[@]}"; do
        block_id="$(trim "${blocks[$j]}")"
        printf '%s' "$(json_escape "$block_id")"
        if [[ "$j" -lt $((${#blocks[@]} - 1)) ]]; then
          printf ', '
        fi
      done
    fi
    printf ']\n'

    if [[ "$i" -lt $((${#issue_ids[@]} - 1)) ]]; then
      printf '    },\n'
    else
      printf '    }\n'
    fi
  done

  printf '  ],\n'
  printf '  "edges": [\n'
  edge_index=0
  edge_total=0
  for deps_csv in "${issue_deps_csv[@]}"; do
    [[ -z "$deps_csv" ]] && continue
    IFS=',' read -r -a deps <<< "$deps_csv"
    edge_total=$((edge_total + ${#deps[@]}))
  done

  for ((i=0; i<${#issue_ids[@]}; i++)); do
    from_id="${issue_ids[$i]}"
    deps_csv="${issue_deps_csv[$i]}"
    [[ -z "$deps_csv" ]] && continue

    IFS=',' read -r -a deps <<< "$deps_csv"
    for dep in "${deps[@]}"; do
      to_id="$(trim "$dep")"
      [[ -z "$to_id" ]] && continue
      printf '    {"from": %s, "to": %s}' "$(json_escape "$from_id")" "$(json_escape "$to_id")"
      edge_index=$((edge_index + 1))
      if [[ "$edge_index" -lt "$edge_total" ]]; then
        printf ',\n'
      else
        printf '\n'
      fi
    done
  done
  printf '  ],\n'
  printf '  "summary": {"total": %d, "completed": %d, "open": %d}\n' "$total_count" "$completed_count" "$open_count"
  printf '}\n'
  exit 0
fi

echo "Issue dependency graph ($issues_dir)"
echo
echo "Summary: total=$total_count, completed=$completed_count, open=$open_count"
echo

for ((i=0; i<${#issue_ids[@]}; i++)); do
  id="${issue_ids[$i]}"
  title="${issue_titles[$i]}"
  completed="${issue_completed[$i]}"
  deps_csv="${issue_deps_csv[$i]}"
  blocks_csv="${issue_blocks_csv[$i]}"

  status="open"
  if [[ "$completed" == "true" ]]; then
    status="done"
  fi

  echo "[$id] $title ($status)"
  if [[ -n "$deps_csv" ]]; then
    IFS=',' read -r -a deps <<< "$deps_csv"
    echo "  depends_on: $(join_by ", " "${deps[@]}")"
  else
    echo "  depends_on: none"
  fi

  if [[ -n "$blocks_csv" ]]; then
    IFS=',' read -r -a blocks <<< "$blocks_csv"
    echo "  blocks: $(join_by ", " "${blocks[@]}")"
  else
    echo "  blocks: none"
  fi
  echo
done
