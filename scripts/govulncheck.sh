#!/usr/bin/env bash
# Fails when govulncheck finds a vulnerability that rumil's code can reach.
#
# GO-2026-6911 has no fixed mcp-go release. Its affected transport is not used
# with a redirect-capable client: the HTTP MCP client refuses redirects, and
# kit sessions are isolated so they load no MCP servers. The advisory allows
# that as a mitigation, so the ID is filtered here and everything else stays
# strict. Remove the exception when mcp-go publishes a fix.
set -euo pipefail

ignored="GO-2026-6911"
report="$(mktemp)"
trap 'rm -f "${report}"' EXIT

status=0
govulncheck -format=json ./... >"${report}" || status=$?
if [ "${status}" -ne 0 ] && [ "${status}" -ne 3 ]; then
	echo "govulncheck failed" >&2
	exit "${status}"
fi

# A finding is reachable when its trace names a function in our code. Findings
# that only name a module are dependencies the code does not call.
reachable="$(jq -r 'select(.finding != null) | .finding | select(any(.trace[]; has("function"))) | .osv' "${report}" | sort -u)"
unexpected="$(printf '%s\n' "${reachable}" | grep -v -x "${ignored}" | grep -v '^$' || true)"

if [ -n "${unexpected}" ]; then
	echo "::error title=Vulnerability check::Reachable vulnerabilities found:"
	printf '%s\n' "${unexpected}"
	govulncheck ./...
	exit 1
fi

if printf '%s\n' "${reachable}" | grep -q -x "${ignored}"; then
	echo "::notice title=Known advisory::${ignored} has no fixed mcp-go release; the MCP HTTP client refuses redirects."
fi
