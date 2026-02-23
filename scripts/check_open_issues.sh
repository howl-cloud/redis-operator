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

if [[ ! -d "$issues_dir" ]]; then
  echo "Issues directory not found: $issues_dir" >&2
  exit 1
fi

shopt -s nullglob
files=("$issues_dir"/*.md)

if [[ ${#files[@]} -eq 0 ]]; then
  if [[ "$output_json" == true ]]; then
    printf '{\n  "issues": [],\n  "count": 0\n}\n'
  else
    echo "No issue files found in $issues_dir"
  fi
  exit 0
fi

rows=()
for file in "${files[@]}"; do
  id=""
  title=""
  completed=""
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
      esac
    fi
  done < "$file"

  completed_lc="$(printf '%s' "$completed" | tr '[:upper:]' '[:lower:]')"
  if [[ "$completed_lc" != "true" ]]; then
    if [[ -z "$completed" ]]; then
      completed="missing"
    fi
    rows+=("${id:-?}|${title:-<no title>}|$completed|$file")
  fi
done

if [[ ${#rows[@]} -eq 0 ]]; then
  if [[ "$output_json" == true ]]; then
    printf '{\n  "issues": [],\n  "count": 0\n}\n'
  else
    echo "All issues are completed (completed: true)."
  fi
  exit 0
fi

if [[ "$output_json" == true ]]; then
  printf '{\n  "issues": [\n'
  for i in "${!rows[@]}"; do
    IFS='|' read -r id title completed file <<< "${rows[$i]}"
    printf '    {\n'
    printf '      "id": "%s",\n' "$(json_escape "$id")"
    printf '      "title": "%s",\n' "$(json_escape "$title")"
    printf '      "completed": "%s",\n' "$(json_escape "$completed")"
    printf '      "file": "%s"\n' "$(json_escape "$file")"
    if [[ "$i" -lt $((${#rows[@]} - 1)) ]]; then
      printf '    },\n'
    else
      printf '    }\n'
    fi
  done
  printf '  ],\n  "count": %d\n}\n' "${#rows[@]}"
  exit 0
fi

printf "%-4s | %-40s | %-10s | %s\n" "ID" "Title" "Completed" "File"
printf "%-4s-+-%-40s-+-%-10s-+-%s\n" "----" "----------------------------------------" "----------" "----"
for row in "${rows[@]}"; do
  IFS='|' read -r id title completed file <<< "$row"
  printf "%-4s | %-40s | %-10s | %s\n" "$id" "$title" "$completed" "$file"
done
