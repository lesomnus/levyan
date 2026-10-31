#!/usr/bin/env bash
# A deployment with data in it, brought up to this build: BEFORE, the build
# this one comes after, makes a database and writes into it what a product and
# an operator write, and NOW migrates it and serves it -- on SQLite, and on
# PostgreSQL when UPGRADE_POSTGRES names an empty database
# (postgres://…/levyan_upgrade?sslmode=disable).
#
#   scripts/upgrade.sh BEFORE NOW
#
# BEFORE and NOW are directories, each with the `levyan` and `levyan-admin` of
# one build: `go build -o DIR/ ./cmd/levyan ./cmd/levyan-admin`.
#
# Every test starts from an empty database, which takes any column the schema
# adds. Only one with rows in it says whether the column can be added to it: a
# column that may not be NULL and has no default cannot be, and the migration
# stops there -- as `Workspace.label` (#25) did on staging. So a field added
# to an entity that has shipped has a default or may be NULL (CLAUDE.md), and
# this is what says whether it does.
#
# What BEFORE writes, each through its door and as its caller: through
# `levyan-admin`, a product and its terms, a stock meter and a flow, a price of
# each, a key, a billing account paid by transfer, and grants; through the API
# with the key, two workspaces arrived, readings and a flow's points in the two
# months before this one and in this one, and an offer; the two months closed
# (`period close`) into statements, and those of the billing account gathered
# into its invoice; and the second workspace closed in the month before this
# one, said after that month was closed, so that it is late (#14). What a build does not have yet -- a command, a method, a
# setting -- it is not asked, and that is said. Nothing here reaches a roster
# or a merchant of record, so what only they make happen is not made: an
# account's own billing accounts and what it answers, a departure, a
# checkout, a subscription, its notices, and an invoice finalized and paid.
#
# What NOW is held to: it migrates the database and says it is in this build's
# shape, and once more with nothing left to do; it serves on it; and what
# BEFORE wrote reads as BEFORE read it -- the product, its key, meters and
# prices, its periods, statements and invoices, the grants (as an operator
# lists them and as payday's `GrantService` answers the product), each
# workspace's payer and what became of it, and what this month's readings come
# to so far -- nothing, of the workspace closed. And if it
# has `levyan trail rows`, every row is what the trail says of it.
#
# Every server this starts is stopped by its PID before this exits. What it
# writes is in a directory of its own, removed when all is well and kept, and
# said, when anything is not.
set -euo pipefail
shopt -s inherit_errexit

before="${1:?usage: scripts/upgrade.sh BEFORE NOW}"
now="${2:?usage: scripts/upgrade.sh BEFORE NOW}"
for b in "${before}" "${now}"; do
	for bin in levyan levyan-admin; do
		if [ ! -x "${b}/${bin}" ]; then
			echo "upgrade: no ${b}/${bin}: BEFORE and NOW are each a directory with one build's levyan and levyan-admin" >&2
			exit 2
		fi
	done
done
for tool in curl jq openssl; do
	if ! command -v "${tool}" >/dev/null; then
		echo "upgrade: ${tool} is needed" >&2
		exit 2
	fi
done

dir="$(mktemp -d)"
echo "upgrade: the databases and their logs are in ${dir}, until all is well"
db=setup
pids=""

# stop: every server this started, by its PID.
stop() {
	local p
	for p in ${pids}; do
		kill "${p}" 2>/dev/null || true
	done
	for p in ${pids}; do
		for _ in $(seq 1 100); do
			kill -0 "${p}" 2>/dev/null || break
			sleep 0.1
		done
		kill -9 "${p}" 2>/dev/null || true
		wait "${p}" 2>/dev/null || true
	done
	pids=""
}
# end: what is left when this exits, however it does.
# shellcheck disable=SC2317 # called by the trap
end() {
	local rc=$?
	stop
	if [ "${rc}" -eq 0 ]; then
		rm -rf "${dir}"
	else
		echo "upgrade: the databases and the logs are in ${dir}" >&2
	fi
}
trap end EXIT
trap 'exit 130' INT TERM

say() { echo "upgrade: ${db}: $*"; }

# fail LOG WHAT...: WHAT said, and the end of LOG, and this stopped.
fail() {
	local log="$1"
	shift
	echo "upgrade: ${db}: $*" >&2
	tail -n 30 "${log}" >&2
	exit 1
}

# What either build is told is this, and nothing of whoever runs it.
for v in $(env | sed -n 's/^\(LEVYAN_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "${v}"
done
export LEVYAN_DB_MIGRATE=false
export LEVYAN_WATCH_BROKER=memory
export LEVYAN_SPIN_LEASE=local
# On addresses the system picks, which each server says in its log.
export LEVYAN_SERVER_ADDR=127.0.0.1:0
export LEVYAN_SERVER_HTTP_ADDR=127.0.0.1:0
export LEVYAN_SERVER_HTTP_ALLOW_WEB=true
export LEVYAN_ADMIN_ADDR=127.0.0.1:0
openssl rand -hex 32 >"${dir}/operator.key"
openssl rand -base64 32 >"${dir}/digest.key"
export LEVYAN_ADMIN_KEYS="upgrade=\${file:${dir}/operator.key}"
export LEVYAN_AUDIT_DIGEST_KEY="${dir}/digest.key"

# reads B NAME: whether build B is told anything through NAME.
reads() { "$1/levyan" config env | grep -qx "$2"; }

# has B COMMAND...: whether build B's levyan-admin has COMMAND.
has() {
	local b="$1"
	shift
	"${b}/levyan-admin" "$@" --help >/dev/null 2>&1
}

# month N: the first moment of the month N months before this one, in UTC --
# the product's zone, which it names none of.
month() {
	local y m
	y="$(date -u +%Y)"
	m=$((10#$(date -u +%m) - $1))
	while [ "${m}" -lt 1 ]; do
		m=$((m + 12))
		y=$((y - 1))
	done
	printf '%04d-%02d-01T00:00:00Z' "${y}" "${m}"
}

# after AT SECONDS: SECONDS after AT, as RFC 3339 says a moment.
after() { jq -rn --arg at "$1" --argjson s "$2" '($at | fromdateiso8601) + $s | todateiso8601'; }

# nanos AT [SECONDS]: AT, and SECONDS after it, in nanoseconds since the epoch,
# as OpenTelemetry says a moment.
nanos() { jq -rn --arg at "$1" --argjson s "${2:-0}" '(($at | fromdateiso8601) + $s | tostring) + "000000000"'; }

# serve B NAME [NAME=VALUE...]: build B's two servers up on the database, told
# what follows NAME besides; the entry point in $url and the API in $http.
serve() {
	local b="$1" name="$2" admin_pid serve_pid
	shift 2
	env "$@" "${b}/levyan-admin" serve >"${dir}/${db}/${name}-admin.log" 2>&1 &
	admin_pid=$!
	pids="${pids} ${admin_pid}"
	env "$@" "${b}/levyan" serve >"${dir}/${db}/${name}-serve.log" 2>&1 &
	serve_pid=$!
	pids="${pids} ${serve_pid}"
	url="http://$(listening "${admin_pid}" "${dir}/${db}/${name}-admin.log" admin)"
	http="http://$(listening "${serve_pid}" "${dir}/${db}/${name}-serve.log" http)"
}

# listening PID LOG WHAT: the address the server PID says in LOG it listens on
# for WHAT, once it has.
listening() {
	local a
	for _ in $(seq 1 600); do
		a="$(sed -n "s/.* $3 - addr=\([^ ]*\).*/\1/p" "$2" | head -n 1)"
		if [ -n "${a}" ]; then
			echo "${a}"
			return
		fi
		kill -0 "$1" 2>/dev/null || fail "$2" "$(basename "$2" .log): stopped before it listened"
		sleep 0.1
	done
	fail "$2" "$(basename "$2" .log): not listening after a minute"
}

# admin B ARGS...: build B's levyan-admin asking the entry point that is up;
# its answer on stdout and in the log.
admin() {
	local b="$1" out
	shift
	echo "\$ levyan-admin $*" >>"${log}"
	if ! out="$("${b}/levyan-admin" --url "${url}" --key "\${file:${dir}/operator.key}" "$@" 2>&1)"; then
		echo "${out}" >>"${log}"
		fail "${log}" "levyan-admin $*: failed"
	fi
	echo "${out}" >>"${log}"
	echo "${out}"
}

# call METHOD BODY: METHOD of the API asked as the product, with its key; the
# answer on stdout -- or 3 and nothing when the build that is up does not
# serve METHOD at all.
call() {
	local code
	echo "\$ $1 $2" >>"${log}"
	code="$(curl -sS -o "${dir}/${db}/answer" -w '%{http_code}' -X POST "${http}/$1" \
		-H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
		-H "Authorization: Bearer ${token}" -d "$2")" || fail "${log}" "$1: no answer"
	cat "${dir}/${db}/answer" >>"${log}"
	echo >>"${log}"
	case "${code}" in
	2??) cat "${dir}/${db}/answer" ;;
	*)
		# A method the build has not got is no route, which answers no
		# error of the protocol's.
		if [ "${code}" = 404 ] && ! jq -e .code "${dir}/${db}/answer" >/dev/null 2>&1; then
			return 3
		fi
		fail "${log}" "$1: ${code}"
		;;
	esac
}

# expect ID LISTING...: what `levyan-admin LISTING` is to list again, as it
# listed it before.
expect() {
	local id="$1"
	shift
	printf '%s\t%s\n' "$*" "${id}" >>"${dir}/${db}/listed"
}

# ask NAME METHOD BODY FILTER: what METHOD answers, as FILTER keeps it, for NOW
# to answer alike.
ask() {
	local answer
	answer="$(call "$2" "$3")"
	printf '%s\t%s\t%s\t%s\n' "$1" "$2" "$3" "$4" >>"${dir}/${db}/asked"
	jq -S "$4" <<<"${answer}" >"${dir}/${db}/$1.before"
}

# The workspaces are the identifiers of a product's payday tenants, and the
# accounts roster's: made up, in payday's shape.
ws1=0190c0de-0000-8000-8001-0000000000a1
ws2=0190c0de-0000-8000-8001-0000000000a2
payer=0190c0de-0000-8000-8002-0000000000c1
other=0190c0de-0000-8000-8002-0000000000c2

# seed: what BEFORE writes, up and then stopped.
seed() {
	local b="${before}" out id ws extra=() billing=""
	if ! "${b}/levyan" migrate >>"${log}" 2>&1; then
		fail "${log}" "the build before did not migrate an empty database"
	fi
	# So that what the close writes is invoiced while this waits.
	if reads "${b}" LEVYAN_INVOICES_EVERY; then
		extra+=(LEVYAN_INVOICES_EVERY=1s)
	fi
	serve "${b}" before "${extra[@]}"

	# The price book, as an operator writes it.
	out="$(admin "${b}" product add --name Loft --yes loft)"
	expect "$(cut -f3 <<<"${out}")" product show loft
	admin "${b}" product configure --channel mor --currency KRW --yes loft >/dev/null
	admin "${b}" meter add --unit '{asset}' --kind stock --aggregation time_weighted_average \
		--name assets --yes loft loft.assets.active >/dev/null
	admin "${b}" meter add --unit By --kind flow --aggregation sum --name transfer --yes loft loft.transfer.bytes >/dev/null
	expect loft.assets.active product show loft
	expect loft.transfer.bytes product show loft
	out="$(admin "${b}" price add --channel mor --currency KRW --included 100 --unit-price 50 --yes loft loft.assets.active)"
	expect "$(cut -f1 <<<"${out}")" price ls loft
	out="$(admin "${b}" price add --channel mor --currency KRW --included 1000 --unit-price 1 --per 1000 \
		--yes loft loft.transfer.bytes)"
	expect "$(cut -f1 <<<"${out}")" price ls loft
	out="$(admin "${b}" key issue --alias upgrade --yes loft)"
	expect "$(head -n 1 <<<"${out}" | cut -f2)" product show loft
	token="$(tail -n 1 <<<"${out}")"

	# A billing account paid by transfer, which an operator opens: the one an
	# account of roster's needs no roster for.
	if has "${b}" billing-account add; then
		out="$(admin "${b}" billing-account add --channel bank_kr --owner "${payer}" --name HQ \
			--registration 123-45-67890 --business-name 'Upgrade Co.' --tax-email tax@example.com --yes)"
		billing="$(cut -f1 <<<"${out}")"
	else
		say "the build before has no \`billing-account add\`: no billing account, and so no invoice"
	fi

	# Two workspaces made two months ago, the first by the account with the
	# billing account, and an offer to it of the second.
	if out="$(call levyan.v1.WorkspaceService/Arrive \
		"{\"workspace\":\"${ws1}\",\"creator\":\"${payer}\",\"created\":\"$(month 2)\",\"label\":\"Upgrade lab\"}")"; then
		call levyan.v1.WorkspaceService/Arrive \
			"{\"workspace\":\"${ws2}\",\"creator\":\"${other}\",\"created\":\"$(month 2)\"}" >/dev/null
		call levyan.v1.WorkspaceService/Offer "{\"workspace\":\"${ws2}\",\"account\":\"${payer}\"}" >/dev/null
		for ws in "${ws1}" "${ws2}"; do
			ask "payer-${ws}" levyan.v1.WorkspaceService/GetPayer "{\"workspace\":\"${ws}\"}" \
				'.payer | {link: .current.id, account: .current.account, billing: .current.billingAccount, start: .current.start, offer: .offer.id}'
		done
	elif [ $? -eq 3 ]; then
		say "the build before has no \`WorkspaceService/Arrive\`: no payers"
	else
		exit 1
	fi

	# A stock's readings at the start of each of the three months, and a
	# flow's points in them.
	out="$(call levyan.v1.MeteringService/Record "$(jq -cn --arg w1 "${ws1}" --arg w2 "${ws2}" \
		--arg m2 "$(month 2)" --arg m1 "$(month 1)" --arg m0 "$(month 0)" '
		def r($ws; $at; $v): {workspace: $ws, meter: "loft.assets.active", measuredAt: $at, value: $v, unit: "{asset}"};
		{readings: [r($w1; $m2; 100), r($w1; $m1; 110), r($w1; $m0; 120), r($w2; $m2; 200), r($w2; $m1; 210), r($w2; $m0; 220)]}')")"
	jq -e 'all(.results[]; .outcome == "OUTCOME_KEPT")' <<<"${out}" >/dev/null || fail "${log}" "readings were refused"
	out="$(call opentelemetry.proto.collector.metrics.v1.MetricsService/Export "$(jq -cn --arg ws "${ws1}" \
		--arg start "$(nanos "$(month 2)")" --arg a "$(nanos "$(month 2)" 3600)" \
		--arg b "$(nanos "$(month 1)" 3600)" --arg c "$(nanos "$(month 0)")" '
		def p($t; $v): {attributes: [{key: "payday.tenant", value: {stringValue: $ws}}], startTimeUnixNano: $start, timeUnixNano: $t, asInt: $v};
		{resourceMetrics: [{resource: {attributes: [{key: "service.name", value: {stringValue: "upgrade"}}]},
			scopeMetrics: [{scope: {name: "loft"}, metrics: [{name: "loft.transfer.bytes", unit: "By",
				sum: {aggregationTemporality: "AGGREGATION_TEMPORALITY_CUMULATIVE", isMonotonic: true,
					dataPoints: [p($a; "5000"), p($b; "9000"), p($c; "12000")]}}]}]}]}')")"
	jq -e '(.partialSuccess.rejectedDataPoints // "0") == "0"' <<<"${out}" >/dev/null || fail "${log}" "points were refused"

	# The two months closed, now rather than after their grace.
	out="$(admin "${b}" period close --yes loft)"
	grep -q $'\t' <<<"${out}" || fail "${log}" "the close closed no period"
	while read -r id; do
		expect "${id}" period ls loft
	done < <(cut -f1 <<<"${out}")
	# What became of the second workspace: closed ten days into the month
	# before this one, and said after that month was closed, as an event
	# that comes late is. Read back as it is, and in what this month comes
	# to so far, which its close takes it out of.
	local rc=0
	call levyan.v1.WorkspaceService/Close "{\"workspace\":\"${ws2}\",\"at\":\"$(after "$(month 1)" 864000)\"}" >/dev/null || rc=$?
	case "${rc}" in
	0)
		for ws in "${ws1}" "${ws2}"; do
			ask "life-${ws}" levyan.v1.WorkspaceService/GetLifecycle "{\"workspace\":\"${ws}\"}" '.lifecycle'
		done
		;;
	3) say "the build before has no \`WorkspaceService/Close\`: no workspace closed" ;;
	*) exit 1 ;;
	esac

	out="$(admin "${b}" statement ls loft)"
	[ -n "${out}" ] || fail "${log}" "the close wrote no statement"
	while read -r id; do
		expect "${id}" statement ls loft
		expect "${id}" statement show "${id}"
	done < <(cut -f1 <<<"${out}")
	for ws in "${ws1}" "${ws2}"; do
		ask "statements-${ws}" levyan.v1.BillingService/ListStatements "{\"workspace\":\"${ws}\"}" \
			'[.statements[] | {id, start: .period.start, total, lines: [.lines[] | {meter, kind, usage, included, billable, amount}]}]'
		ask "usage-${ws}" levyan.v1.BillingService/GetUsage "{\"workspace\":\"${ws}\"}" \
			'.usage | {period: .period.start, lines: [.lines[] | {meter, usage}]}'
	done

	# What the statements of the billing account came to, gathered by the
	# loop onto its draft.
	if [ -n "${billing}" ] && has "${b}" invoice ls; then
		for _ in $(seq 1 60); do
			out="$(admin "${b}" invoice ls loft)"
			[ -z "${out}" ] || break
			sleep 0.5
		done
		[ -n "${out}" ] || fail "${log}" "nothing the close wrote was invoiced within half a minute"
		while read -r id; do
			expect "${id}" invoice ls loft
			expect "${id}" invoice show "${id}"
		done < <(cut -f1 <<<"${out}")
	fi

	if has "${b}" grant add; then
		out="$(admin "${b}" grant add --workspace "${ws1}" --on --source trial --desc pilot --yes loft loft.export)"
		expect "$(cut -f1 <<<"${out}")" grant ls loft
		out="$(admin "${b}" grant add --workspace "${ws2}" --limit 20 --until 2099-01-01 --source contract --yes loft loft.seats)"
		expect "$(cut -f1 <<<"${out}")" grant ls loft
		ask grants payday.GrantService/List '{}' \
			'[.items[] | {id, tenantId, key, version, on, limit, unlimited, validFrom, validUntil}] | sort_by(.id)'
	else
		say "the build before has no \`grant add\`: no grants"
	fi

	stop
}

# migrate: NOW brings the database into its shape and says so, and then once
# more, with nothing left to do.
migrate() {
	local out
	for _ in 1 2; do
		echo "\$ levyan migrate" >>"${log}"
		if ! out="$("${now}/levyan" migrate 2>&1)"; then
			echo "${out}" >>"${log}"
			fail "${log}" "this build did not migrate what the build before made"
		fi
		echo "${out}" >>"${log}"
		grep -q "the database is in this build's shape" <<<"${out}" ||
			fail "${log}" "this build's migrate did not say the database is in its shape"
	done
}

# reread: what NOW, serving, says of what BEFORE wrote.
reread() {
	local listing id name method body filter answer rc n=0 m=0
	local -A listed=()
	while IFS=$'\t' read -r -u 3 listing id; do
		if [ -z "${listed[${listing}]+x}" ]; then
			# shellcheck disable=SC2086 # a listing is its words
			listed[${listing}]="$(admin "${now}" ${listing})"
		fi
		grep -qF -- "${id}" <<<"${listed[${listing}]}" ||
			fail "${log}" "levyan-admin ${listing}: ${id} is not in it, which the build before wrote"
		n=$((n + 1))
	done 3<"${dir}/${db}/listed"

	while IFS=$'\t' read -r -u 3 name method body filter; do
		rc=0
		answer="$(call "${method}" "${body}")" || rc=$?
		case "${rc}" in
		0) ;;
		3) fail "${log}" "${method}: not served, which the build before was" ;;
		*) exit 1 ;;
		esac
		jq -S "${filter}" <<<"${answer}" >"${dir}/${db}/${name}.now"
		if ! diff -u "${dir}/${db}/${name}.before" "${dir}/${db}/${name}.now" >>"${log}"; then
			# What this month comes to is this month's: a month that turned
			# between the two builds is another question.
			if [ "${name#usage-}" != "${name}" ] &&
				[ "$(jq -r .period "${dir}/${db}/${name}.before")" != "$(jq -r .period "${dir}/${db}/${name}.now")" ]; then
				say "the month turned between the two builds, so what ${body} comes to this month is not compared"
				continue
			fi
			fail "${log}" "${method} ${body}: answered otherwise than the build before did"
		fi
		m=$((m + 1))
	done 3<"${dir}/${db}/asked"

	# Every row what the trail's digest of it says: written through the
	# servers by the build before, and rewritten by the migration on the
	# trail or not at all.
	if "${now}/levyan" trail rows --help >/dev/null 2>&1; then
		echo "\$ levyan trail rows" >>"${log}"
		"${now}/levyan" trail rows >>"${log}" 2>&1 || fail "${log}" "levyan trail rows: a row is not what the trail says of it"
	else
		say "this build has no \`levyan trail rows\`, so whether each row is what the trail says of it is not asked"
	fi

	say "${n} rows listed again and ${m} answers alike"
}

# check NAME DRIVER DSN: what BEFORE makes on the database DSN names, migrated
# by NOW and read by it -- in a shell of its own, which stops its own servers,
# so that one database that fails does not keep the other from saying what
# it does. Answers whether all was well.
check() {
	(
		set -e
		trap stop EXIT
		trap 'exit 130' INT TERM
		db="$1"
		export LEVYAN_DB_DRIVER="$2"
		export LEVYAN_DB_DSN="$3"
		mkdir -p "${dir}/${db}"
		log="${dir}/${db}/commands.log"
		: >"${dir}/${db}/listed"
		: >"${dir}/${db}/asked"
		seed
		migrate
		serve "${now}" now
		reread
		stop
		say "what the build before wrote is in this build's shape, and reads as it did"
	)
}

# Each check called outside of `set -e`, and of any condition -- inside one,
# `set -e` would be off in the check's own shell too.
status=0
set +e
check sqlite sqlite3 "file:${dir}/sqlite/levyan.db?_pragma=foreign_keys(1)"
status=$((status | $?))
if [ -n "${UPGRADE_POSTGRES:-}" ]; then
	check postgres pgx "${UPGRADE_POSTGRES}"
	status=$((status | $?))
else
	db=postgres
	say "UPGRADE_POSTGRES names no database, so it is not asked"
fi
set -e
exit "${status}"
