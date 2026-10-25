#!/usr/bin/env bash
# migrate-notify.sh <config.toml>...
#
# Rewrites [supervise] notify and [supervise.ntfy], which drover refuses at
# load, into [notify] destinations, in place. Works on a project's .drover.toml
# and on the per-user config.toml alike.
#
#   [supervise] notify = [...]  becomes  [notify.destinations.command], routed
#                                        { needs_operator = "default" }
#   [supervise.ntfy]            becomes  [notify.destinations.ntfy], carrying
#                                        url, token_file, and each of its events
#                                        at its priority (needs_operator at high
#                                        where it named neither)
#
# Only those lines change: comments and every other table stay byte for byte.
# The result goes through a temp file and a rename, through a symlink to the
# file it points at. A file carrying neither key is left untouched, and so is
# one carrying a form this script does not rewrite (an inline or dotted ntfy
# table), which it names and exits 1 on.
set -euo pipefail

me=$(basename "$0")
[ $# -ge 1 ] || { echo "usage: $me <config.toml>..." >&2; exit 2; }

# resolve follows a symlink to the file it names, so a config kept in a
# dotfiles repository is rewritten there rather than replaced by a copy.
resolve() {
	local f=$1 link
	while [ -L "$f" ]; do
		link=$(readlink "$f")
		case $link in
		/*) f=$link ;;
		*) f=$(dirname "$f")/$link ;;
		esac
	done
	printf '%s\n' "$f"
}

# Exit 0 rewrote, 3 found nothing to migrate, 1 refused with a reason on stderr.
read -r -d '' prog <<'AWK' || true
function die(msg) { print FILENAME ": " msg > "/dev/stderr"; dead = 1; exit 1 }
function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t\r]+$/, "", s); return s }
# header is the table a [table] line opens, with spaces and a trailing comment
# dropped, or empty for any other line.
function header(line,   h) {
	h = trim(line)
	if (h !~ /^\[/) return ""
	sub(/\][ \t]*#.*$/, "]", h)
	gsub(/[ \t]/, "", h)
	return h
}
# depth is the brackets a line opens less those it closes, outside strings and
# comments, which is how far a multi-line array is from closing. ml carries a
# multi-line string (''' or """) open at the end of one line into the next,
# since an argv holding a script is usually written as one.
function depth(s,   i, c, d, q, three) {
	d = 0; q = ""
	for (i = 1; i <= length(s); i++) {
		c = substr(s, i, 1)
		three = substr(s, i, 3)
		if (ml != "") {
			if (c == "\\" && ml == "\"\"\"") i++
			else if (three == ml) { ml = ""; i += 2 }
		} else if (q != "") {
			if (c == "\\" && q == "\"") i++
			else if (c == q) q = ""
		} else if (three == "'''" || three == "\"\"\"") { ml = three; i += 2 }
		else if (c == "\"" || c == "'") q = c
		else if (c == "#") break
		else if (c == "[") d++
		else if (c == "]") d--
	}
	return d
}
NR == FNR {
	h = header($0)
	if (h != "" && cap == "") {
		tbl = h
		if (h == "[notify.destinations.command]") hasCommand = 1
		if (h == "[notify.destinations.ntfy]") hasNtfy = 1
		if (h == "[supervise.ntfy]") {
			ntfyHead = FNR
			if (match($0, /\][ \t]*#.*$/)) ntfyComment = substr($0, RSTART + 1)
		}
		if (h ~ /^\[supervise\.ntfy\./) die("line " FNR ": " h " sits under [supervise.ntfy]; rewrite it by hand")
		next
	}
	if (cap != "") {
		skip[FNR] = 1
		if (cap == "notify") notifyText = notifyText "\n" $0
		else eventsText = eventsText " " $0
		open += depth($0)
		if (open <= 0) cap = ""
		next
	}
	if (tbl == "" && $0 ~ /^[ \t]*supervise[ \t]*[.=]/) die("line " FNR ": supervise is written as a dotted key or an inline table; rewrite it by hand")
	if (tbl == "[supervise]" && $0 ~ /^[ \t]*ntfy[ \t]*[.=]/) die("line " FNR ": ntfy is written inside [supervise] rather than as [supervise.ntfy]; rewrite it by hand")
	if (tbl == "[supervise]" && $0 ~ /^[ \t]*notify[ \t]*=/) {
		hasNotify = 1
		skip[FNR] = 1
		notifyText = $0
		sub(/^[ \t]*notify[ \t]*=/, "argv =", notifyText)
		open = depth($0)
		if (open > 0) cap = "notify"
		next
	}
	if (tbl == "[supervise.ntfy]" && $0 ~ /^[ \t]*events[ \t]*=/) {
		eventsLine = FNR
		eventsText = $0
		sub(/^[ \t]*events[ \t]*=/, "", eventsText)
		open = depth($0)
		if (open > 0) cap = "events"
		next
	}
	if (tbl == "[supervise.ntfy]" && $0 ~ /^[ \t]*priority[ \t]*=/) {
		skip[FNR] = 1
		priority = $0
		if (!match(priority, /"[^"]*"|'[^']*'/)) die("line " FNR ": priority is not a quoted string")
		priority = substr(priority, RSTART + 1, RLENGTH - 2)
	}
	next
}
FNR == 1 {
	if (dead) exit 1
	if (!hasNotify && !ntfyHead) exit 3
	if (hasNotify && hasCommand) die("[notify.destinations.command] exists already; rename it, then run this again")
	if (ntfyHead && hasNtfy) die("[notify.destinations.ntfy] exists already; rename it, then run this again")
	if (priority == "") priority = "high"
	routed = ""
	if (eventsLine) {
		rest = eventsText
		while (match(rest, /"[^"]*"|'[^']*'/)) {
			name = substr(rest, RSTART + 1, RLENGTH - 2)
			routed = routed (routed == "" ? "" : ", ") name " = \"" priority "\""
			rest = substr(rest, RSTART + RLENGTH)
		}
	} else routed = "needs_operator = \"" priority "\""
}
FNR == ntfyHead {
	print "[notify.destinations.ntfy]" ntfyComment
	print "type = \"ntfy\""
	if (!eventsLine) print "events = { " routed " }"
	next
}
FNR == eventsLine { print "events = { " routed " }"; next }
skip[FNR] { next }
{ print }
END {
	if (dead) exit 1
	if (hasNotify) {
		print ""
		print "[notify.destinations.command]"
		print "type = \"command\""
		print notifyText
		print "events = { needs_operator = \"default\" }"
	}
}
AWK

for arg in "$@"; do
	file=$(resolve "$arg")
	[ -f "$file" ] || { echo "$me: $arg: no such file" >&2; exit 1; }
	tmp=$(mktemp "$file.migrate.XXXXXX")
	# cp -p first, so the rename keeps the file's mode rather than mktemp's 0600.
	cp -p "$file" "$tmp"
	status=0
	awk "$prog" "$file" "$file" >"$tmp" || status=$?
	case $status in
	0)
		mv "$tmp" "$file"
		echo "$me: $arg: rewritten; check it with drover config, then drover notify --test"
		;;
	3)
		rm -f "$tmp"
		echo "$me: $arg: no [supervise] notify or [supervise.ntfy]; unchanged"
		;;
	*)
		rm -f "$tmp"
		exit 1
		;;
	esac
done
