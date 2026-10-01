#!/usr/bin/env bash
# Makes APT on a GitHub-hosted Ubuntu runner try archive.ubuntu.com before
# azure.archive.ubuntu.com.  CI calls it right before `apt-get update`.
#
# The runner image (actions/runner-images,
# images/ubuntu/scripts/build/configure-apt-sources.sh) points the Ubuntu
# sources at mirror+file:/etc/apt/apt-mirrors.txt, which lists:
#
#   http://azure.archive.ubuntu.com/ubuntu/<TAB>priority:1
#   https://archive.ubuntu.com/ubuntu/<TAB>priority:2
#   https://security.ubuntu.com/ubuntu/<TAB>priority:3
#
# APT's mirror method uses the lowest priority value first, shuffles equal
# values, and keeps the rest as failover alternates.  Some Azure mirror nodes
# send large packages slowly without ever going idle, so APT never fails over.
#
# This script changes only the priority numbers: archive.ubuntu.com gets 1,
# azure.archive.ubuntu.com gets 2, and every other entry keeps its relative
# order after them.  URIs, suites, components, signing keys, and the package
# set are untouched, and every mirror remains a fallback.
#
# Anything other than that exact layout (no GitHub-hosted runner, the file not
# referenced by a source, an unknown line, a missing or duplicated host) is
# left alone with a ::notice:: and exit 0, so APT runs with the image's
# configuration.  Failing to write a recognised file fails the step.
#
# APT_MIRRORS_FILE and APT_SOURCES_DIR override the paths for fixture tests.
set -euo pipefail

mirrors=${APT_MIRRORS_FILE:-/etc/apt/apt-mirrors.txt}
sources_dir=${APT_SOURCES_DIR:-/etc/apt}

skip() {
  echo "::notice title=apt-mirror::leaving APT mirrors unchanged: $*"
  exit 0
}

[ "${RUNNER_ENVIRONMENT:-}" = github-hosted ] || skip "not a GitHub-hosted runner"
[ -f "${mirrors}" ] || skip "${mirrors} does not exist"

referenced=false
for source in "${sources_dir}/sources.list" "${sources_dir}"/sources.list.d/*.list \
  "${sources_dir}"/sources.list.d/*.sources; do
  if [ -f "${source}" ] && grep -qF "mirror+file:${mirrors}" "${source}"; then
    referenced=true
    break
  fi
done
[ "${referenced}" = true ] || skip "no APT source uses mirror+file:${mirrors}"

# Rank: archive.ubuntu.com, then azure.archive.ubuntu.com, then the others by
# their original priority and line order.  Lines stay where they are.
if ! ranked=$(awk -F '\t' '
  function host(uri) { sub(/^[a-z]+:\/\//, "", uri); sub(/\/.*/, "", uri); return uri }
  /^(#|$)/ { line[NR] = $0; next }
  NF != 2 || $1 !~ /^https?:\/\/[^[:space:]]+$/ || $2 !~ /^priority:[0-9]+$/ {
    print "unexpected line " NR ": " $0 > "/dev/stderr"; bad = 1; exit
  }
  {
    h = host($1)
    tier = h == "archive.ubuntu.com" ? 0 : h == "azure.archive.ubuntu.com" ? 1 : 2
    count[tier]++
    n++; row[n] = NR; uri[NR] = $1; key[n] = sprintf("%d %020d %020d", tier, substr($2, 10), NR)
  }
  END {
    if (bad) exit 1
    if (count[0] != 1 || count[1] != 1) {
      print "expected one archive.ubuntu.com and one azure.archive.ubuntu.com entry" > "/dev/stderr"
      exit 1
    }
    for (i = 2; i <= n; i++)
      for (j = i; j > 1 && key[j - 1] > key[j]; j--) {
        t = key[j]; key[j] = key[j - 1]; key[j - 1] = t
        t = row[j]; row[j] = row[j - 1]; row[j - 1] = t
      }
    for (i = 1; i <= n; i++) line[row[i]] = uri[row[i]] "\tpriority:" i
    for (i = 1; i <= NR; i++) print line[i]
  }' "${mirrors}" 2>&1); then
  skip "${mirrors}: ${ranked}"
fi

if [ "${ranked}" = "$(cat "${mirrors}")" ]; then
  echo "::notice title=apt-mirror::archive.ubuntu.com already has the highest priority"
  exit 0
fi

tmp=$(mktemp)
trap 'rm -f "${tmp}"' EXIT
printf '%s\n' "${ranked}" >"${tmp}"
if [ -w "${mirrors}" ]; then
  install -m 0644 "${tmp}" "${mirrors}"
else
  sudo install -m 0644 "${tmp}" "${mirrors}"
fi
echo "::notice title=apt-mirror::archive.ubuntu.com now has the highest priority in ${mirrors}"
cat "${mirrors}"
