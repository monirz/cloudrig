#!/bin/sh
# Runs govulncheck and fails only on a vulnerability this module can actually
# reach that is NOT explicitly allowed.
#
# govulncheck exits non-zero when your code CALLS a vulnerable symbol. The two
# allowed IDs below are the Moby daemon's — an AuthZ plugin-authorization bypass
# and a plugin-privilege off-by-one — in github.com/docker/docker. cloudrig uses
# that module only as a client (create/start/inspect a container), never as a
# daemon, so the vulnerable server-side plugin paths are unreachable here. Both
# have Fixed in: N/A, so there is no upgrade that clears them; they are pinned
# by ID with this reasoning rather than left to turn the job permanently red.
#
# Any OTHER reachable vulnerability fails the job, so a real one is never masked.
set -eu

# Pinned, not @latest: CI must run the reviewed scanner, not whatever ships
# next. Bump this deliberately and let the change go through review.
GOVULNCHECK=golang.org/x/vuln/cmd/govulncheck@v1.7.0

ALLOW="GO-2026-4887 GO-2026-4883"

out=$(GOWORK=off go run "$GOVULNCHECK" ./... 2>&1) || true
printf '%s\n' "$out"

# The IDs govulncheck reports under Symbol Results are the ones the code calls.
called=$(printf '%s\n' "$out" | sed -n 's/^Vulnerability #[0-9]*: \(GO-[0-9-]*\).*/\1/p' | sort -u)

status=0
for id in $called; do
	case " $ALLOW " in
		*" $id "*) ;;                        # allowed, daemon-side, unreachable
		*)
			echo "vuln: unexpected reachable vulnerability: $id"
			status=1
			;;
	esac
done

if [ "$status" -eq 0 ] && [ -n "$called" ]; then
	echo "vuln: only allow-listed daemon-side Docker findings remain ($ALLOW); OK."
fi
exit $status
