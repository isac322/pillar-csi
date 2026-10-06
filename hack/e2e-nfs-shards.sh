#!/usr/bin/env bash
# e2e-nfs-shards.sh — run and verify the native NFS e2e lane as sequential
# TC-focused shards.
#
# SHARD_TABLE is the single canonical shard definition.  Each shard runs as
# its own `make test-e2e-internal` invocation, so the harness creates and
# deletes a fresh Kind cluster per shard while each invocation keeps the
# unchanged per-invocation budgets (20m Ginkgo suite timeout, E2E_TIMEOUT go
# test timeout).  Ordered containers are never split across shards.
#
# Commands:
#   list                              shard names in execution order
#   ids [shard]                       expected TC IDs (every shard when omitted)
#   focus <shard>                     TC focus regex passed as go test -run
#   coverage                          shard table vs It("[TC-...]") declarations
#   check <shard> <report>            validate a native Ginkgo JSON report
#   check-selection <shard> <report>  validate a dry-run report selects exactly
#                                     the shard (never accepted as native proof)
#   run <report-root>                 run every shard, validate every report,
#                                     write the step summary; fails on any problem
#
# Exit codes: 0 success, 1 verification failure, 2 usage/environment error.
set -uo pipefail

# name  family  id-ranges
SHARD_TABLE='
e37-dataset        E37  1-13
e71-local          E71  1-16,29
e71-net-basic      E71  17-21
e71-net-recovery   E71  22-24
e71-net-lifecycle  E71  25-26
e71-net-packaging  E71  27-28
'

# Per-invocation Ginkgo suite timeout.  A TC focus bypasses TestE2E's
# E2E_NFS_E2E 20m cap, so the same bound is passed explicitly and native
# reports must record exactly this SuiteConfig.Timeout (nanoseconds).
GINKGO_SUITE_TIMEOUT_MINUTES=20
GINKGO_SUITE_TIMEOUT="${GINKGO_SUITE_TIMEOUT_MINUTES}m"
GINKGO_SUITE_TIMEOUT_NS=$((GINKGO_SUITE_TIMEOUT_MINUTES * 60 * 1000000000))

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)

die() {
	echo "e2e-nfs-shards: $*" >&2
	exit 2
}

shard_names() {
	awk 'NF { print $1 }' <<<"$SHARD_TABLE"
}

shard_ids() {
	local name family ranges range lo hi i found=
	local -a parts
	while read -r name family ranges; do
		[ "$name" = "$1" ] || continue
		found=1
		IFS=, read -ra parts <<<"$ranges"
		for range in "${parts[@]}"; do
			lo=${range%-*}
			hi=${range#*-}
			for ((i = lo; i <= hi; i++)); do
				echo "TC-$family.$i"
			done
		done
	done <<<"$SHARD_TABLE"
	[ -n "$found" ]
}

all_ids() {
	local name
	for name in $(shard_names); do
		shard_ids "$name"
	done
}

# TC-(?:E71\.17|E71\.18)\] — starts with "TC-" so TestMain routes it to the
# Ginkgo focus; the literal closing bracket keeps TC-E71.1 from matching
# TC-E71.17.
shard_focus() {
	local ids alt
	ids=$(shard_ids "$1") || return 1
	alt=$(sed -e 's/^TC-//' -e 's/\./\\./g' <<<"$ids" | paste -sd'|' -)
	printf 'TC-(?:%s)\\]\n' "$alt"
}

cmd_coverage() {
	local families declared assigned problems=
	families=$(awk 'NF { print $2 }' <<<"$SHARD_TABLE" | sort -u | paste -sd'|' -)
	declared=$(grep -rhoE "It\\(\"\\[TC-($families)\\.[0-9]+\\]" "$REPO_ROOT/test/e2e" --include='*.go' |
		sed -E 's/^It\("\[//; s/\]$//' | sort)
	assigned=$(all_ids | sort)
	problems+=$(uniq -d <<<"$assigned" | sed 's/$/: assigned to more than one shard/')
	problems+=$'\n'$(uniq -d <<<"$declared" | sed 's/$/: declared by more than one It/')
	problems+=$'\n'$(comm -23 <(sort -u <<<"$declared") <(sort -u <<<"$assigned") | sed 's/$/: declared but not assigned to a shard/')
	problems+=$'\n'$(comm -13 <(sort -u <<<"$declared") <(sort -u <<<"$assigned") | sed 's/$/: assigned but not declared/')
	problems=$(grep -v '^$' <<<"$problems")
	if [ -n "$problems" ]; then
		printf '%s\n' "$problems"
		return 1
	fi
	echo "coverage: $(wc -l <<<"$assigned" | tr -d ' ') TC IDs across $(shard_names | wc -l | tr -d ' ') shards match the declarations"
}

# Inputs: $mode ("native" or "selection"), $expected (array of TC IDs),
# $timeout_ns (required native SuiteConfig.Timeout).
# Output: {ran, passed, problems[]}.  Specs outside the focus are reported by
# Ginkgo as "skipped"; every other It spec counts as executed.
read -r -d '' JQ_CHECK <<'JQ'
def tcid: ((.LeafNodeText // "") | capture("\\[(?<id>TC-[^\\]]+)\\]").id) // "<untagged>";
if type != "array" then {ran: 0, passed: 0, problems: ["report is not a JSON array of suite reports"]}
elif length != 1 then {ran: 0, passed: 0, problems: ["expected exactly 1 suite report, found \(length)"]}
else
  .[0] as $r
  | [($r.SpecReports // [])[] | select(.LeafNodeType == "It")] as $its
  | [$its[] | select(.State != "skipped")] as $ran
  | {
      ran: ($ran | length),
      passed: ([$ran[] | select(.State == "passed")] | length),
      problems: (
        (if $mode == "native" then
           (if $r.SuiteConfig.DryRun != false then ["SuiteConfig.DryRun=\($r.SuiteConfig.DryRun | tojson): native proof requires an explicit false"] else [] end)
           + (if $r.SuiteConfig.Timeout != $timeout_ns then ["SuiteConfig.Timeout=\($r.SuiteConfig.Timeout | tojson): native proof requires \($timeout_ns) (Ginkgo suite timeout)"] else [] end)
           + (if $r.SuiteSucceeded != true then ["SuiteSucceeded=\($r.SuiteSucceeded) \(($r.SpecialSuiteFailureReasons // []) | join("; "))"] else [] end)
           + [($r.SpecReports // [])[] | select(.LeafNodeType != "It" and .State != "passed" and .State != "skipped") | "\(.LeafNodeType) node state=\(.State)"]
         else
           (if $r.SuiteConfig.DryRun == true then [] else ["SuiteConfig.DryRun=\($r.SuiteConfig.DryRun | tojson): selection check requires a dry-run report"] end)
         end)
        + [$expected[] as $id
            | [$its[] | select(tcid == $id)] as $m
            | if ($m | length) == 0 then "\($id): missing from report"
              elif ($m | length) > 1 then "\($id): duplicated (\($m | length) specs)"
              elif $mode == "native" and $m[0].State != "passed" then "\($id): state=\($m[0].State)"
              elif $mode == "selection" and $m[0].State == "skipped" then "\($id): not selected"
              else empty end]
        + [$ran[] | tcid as $id | select(any($expected[]; . == $id) | not) | "\($id): unexpected spec executed (state=\(.State))"]
      )
    }
end
JQ

# check_report <native|selection> <shard> <report>
# Sets CHECK_RAN, CHECK_PASSED, CHECK_PROBLEMS; returns non-zero on problems.
check_report() {
	local mode=$1 shard=$2 report=$3 ids expected result
	CHECK_RAN=0
	CHECK_PASSED=0
	CHECK_PROBLEMS=
	if ! ids=$(shard_ids "$shard"); then
		CHECK_PROBLEMS="unknown shard: $shard"
		return 1
	fi
	if [ ! -s "$report" ]; then
		CHECK_PROBLEMS="report missing or empty: $report"
		return 1
	fi
	expected=$(jq -Rn '[inputs]' <<<"$ids")
	if ! result=$(jq -c --arg mode "$mode" --argjson expected "$expected" --argjson timeout_ns "$GINKGO_SUITE_TIMEOUT_NS" "$JQ_CHECK" "$report" 2>&1); then
		CHECK_PROBLEMS="report unreadable: $result"
		return 1
	fi
	CHECK_RAN=$(jq -r '.ran' <<<"$result")
	CHECK_PASSED=$(jq -r '.passed' <<<"$result")
	CHECK_PROBLEMS=$(jq -r '.problems[]' <<<"$result")
	[ -z "$CHECK_PROBLEMS" ]
}

cmd_check() {
	local mode=$1 shard=$2 report=$3 expected
	expected=$(shard_ids "$shard" | wc -l | tr -d ' ')
	if check_report "$mode" "$shard" "$report"; then
		echo "PASS $shard: $CHECK_PASSED/$expected expected specs passed, $CHECK_RAN executed"
		return 0
	fi
	echo "FAIL $shard: $CHECK_PASSED/$expected expected specs passed, $CHECK_RAN executed"
	sed 's/^/  - /' <<<"$CHECK_PROBLEMS"
	return 1
}

cmd_run() {
	local root=$1 name dir focus rc log_rc expected status=0 coverage verdict
	local -a pipe_rc
	local -a rows=() details=()
	mkdir -p "$root" || die "cannot create $root"
	cd "$REPO_ROOT" || die "cannot enter $REPO_ROOT"

	if ! coverage=$(cmd_coverage); then
		status=1
		details+=("coverage" "$coverage")
		while IFS= read -r line; do
			echo "::error title=nfs-shard-coverage::$line"
		done <<<"$coverage"
	fi
	echo "$coverage"

	# Every shard runs even after a failure; the verdict is decided at the end.
	for name in $(shard_names); do
		dir="$root/$name"
		mkdir -p "$dir"
		rm -f "$dir/e2e-auto.json" "$dir/run.log"
		focus=$(shard_focus "$name")
		expected=$(shard_ids "$name" | wc -l | tr -d ' ')
		echo "::group::NFS shard $name ($expected specs)"
		echo "e2e-nfs-shards: $name focus $focus"
		# The focus is single-quoted inside E2E_GO_FLAGS because the Makefile
		# expands E2E_GO_FLAGS unquoted into the recipe shell.
		E2E_REPORT_DIR="$dir" make \
			"E2E_GO_FLAGS=-tags=e2e,e2e_helm ./test/e2e/ -v -timeout=\$(E2E_TIMEOUT) -ginkgo.timeout=$GINKGO_SUITE_TIMEOUT -run '$focus'" \
			test-e2e-internal 2>&1 | tee "$dir/run.log"
		pipe_rc=("${PIPESTATUS[@]}")
		rc=${pipe_rc[0]}
		log_rc=${pipe_rc[1]}
		echo "::endgroup::"

		verdict=PASS
		check_report native "$name" "$dir/e2e-auto.json" || verdict=FAIL
		if [ "$rc" -ne 0 ]; then
			verdict=FAIL
			CHECK_PROBLEMS=$(printf 'make test-e2e-internal exited with status %s\n%s' "$rc" "$CHECK_PROBLEMS")
		fi
		if [ "$log_rc" -ne 0 ]; then
			verdict=FAIL
			CHECK_PROBLEMS=$(printf 'tee to run log %s exited with status %s\n%s' "$dir/run.log" "$log_rc" "$CHECK_PROBLEMS")
		fi
		CHECK_PROBLEMS=$(grep -v '^$' <<<"$CHECK_PROBLEMS")
		rows+=("| $name | $expected | $CHECK_RAN | $CHECK_PASSED | $rc | $verdict |")
		if [ "$verdict" = FAIL ]; then
			status=1
			details+=("$name" "$CHECK_PROBLEMS")
			while IFS= read -r line; do
				echo "::error title=nfs-shard-$name::$line"
			done <<<"$CHECK_PROBLEMS"
		else
			echo "::notice title=nfs-shard-$name::$CHECK_PASSED/$expected expected specs passed"
		fi
	done

	{
		echo "## NFS E2E Shards"
		echo ""
		echo "| Shard | Expected | Executed | Passed | Exit | Status |"
		echo "|-------|----------|----------|--------|------|--------|"
		printf '%s\n' "${rows[@]}"
		local i
		for ((i = 0; i < ${#details[@]}; i += 2)); do
			echo ""
			echo "**${details[i]}**"
			echo ""
			sed 's/^/- /' <<<"${details[i + 1]}"
		done
	} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
	pipe_rc=("${PIPESTATUS[@]}")
	if [ "${pipe_rc[1]}" -ne 0 ]; then
		echo "::error title=nfs-shard-summary::tee to step summary ${GITHUB_STEP_SUMMARY:-/dev/null} exited with status ${pipe_rc[1]}"
		status=1
	fi
	return "$status"
}

command -v jq >/dev/null 2>&1 || die "jq is required"

case "${1:-}" in
list) shard_names ;;
ids)
	if [ $# -ge 2 ]; then
		shard_ids "$2" || die "unknown shard: $2"
	else
		all_ids
	fi
	;;
focus)
	[ $# -eq 2 ] || die "usage: $0 focus <shard>"
	shard_focus "$2" || die "unknown shard: $2"
	;;
coverage) cmd_coverage ;;
check)
	[ $# -eq 3 ] || die "usage: $0 check <shard> <report>"
	cmd_check native "$2" "$3"
	;;
check-selection)
	[ $# -eq 3 ] || die "usage: $0 check-selection <shard> <report>"
	cmd_check selection "$2" "$3"
	;;
run)
	[ $# -eq 2 ] || die "usage: $0 run <report-root>"
	cmd_run "$2"
	;;
*) die "usage: $0 {list|ids [shard]|focus <shard>|coverage|check <shard> <report>|check-selection <shard> <report>|run <report-root>}" ;;
esac
