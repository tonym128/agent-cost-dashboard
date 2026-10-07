# syntax=docker/dockerfile:1
#
# dashd — self-hosted cost dashboard for AI coding agents.
#
# Runtime base is Alpine, deliberately, even though the binary is static and
# would run on distroless or scratch. The reason is HEALTHCHECK: distroless and
# scratch have no shell and no wget, so a `HEALTHCHECK CMD wget ...` is
# impossible there, and the alternative — a separate Go binary used only as a
# probe — is more moving parts than this project wants to own. Alpine gives us
# a working in-image healthcheck, a real /etc/passwd entry for the non-root
# user, and a shell for debugging. ~4MB over distroless is a fair trade.
#
# NOTE for anyone switching to distroless: drop the HEALTHCHECK below and
# configure the orchestrator's HTTP probe against /healthz instead, which
# returns the stored totals and last scan time.

# ---------------------------------------------------------------------------
# Stage 1: build the static binary.
# ---------------------------------------------------------------------------
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first, so a source-only change reuses the cached layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 with modernc.org/sqlite (a pure-Go SQLite) yields a statically
# linked binary: no libc, no musl, nothing needed at runtime.
#
# -trimpath removes local filesystem paths from the binary so the build does
# not depend on where it ran. The three -X flags below set the version, commit
# and build date; cmd/dashd declares all three in version.go, so they take
# effect. (They did not, once: Go silently discards an -X for an undeclared
# symbol, which is why version.go now exists and documents the trap.)
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
          -X main.version=${VERSION} \
          -X main.commit=${COMMIT} \
          -X main.date=${DATE}" \
        -o /out/dashd ./cmd/dashd

# ---------------------------------------------------------------------------
# Stage 2: runtime.
# ---------------------------------------------------------------------------
FROM alpine:3.22

# ca-certificates for future HTTPS use; tzdata so timestamps in the dashboard
# render in a real local zone rather than UTC-by-accident.
RUN apk add --no-cache ca-certificates tzdata

# ---------------------------------------------------------------------------
# Non-root user.
#
# dashd reads the agent's log files out of a home directory, and those are
# typically mode 0700 owned by the human running the agents. A container user
# with a different uid therefore cannot read them, and the dashboard silently
# reports zero activity — the same "confidently wrong" failure mode as missing
# pricing, so it is worth being explicit.
#
# The fix is not to run as root. It is to make the container's uid match the
# host uid of the person whose logs you want to read, and to give that uid a
# writable HOME. This exact invocation is verified to work:
#
#     mkdir -p ~/.dashd-home && chmod 0777 ~/.dashd-home
#     docker run --user "$(id -u):$(id -g)" \
#       -v "$HOME/.dashd-home:/home/dashd" \
#       -v "$HOME/.claude:/home/dashd/.claude:ro" \
#       -v "$HOME/.codex:/home/dashd/.codex:ro" \
#       -v "$HOME/.gemini:/home/dashd/.gemini:ro" \
#       -v "$HOME/.pi:/home/dashd/.pi:ro" \
#       -v "$HOME/.local/share/opencode:/home/dashd/.local/share/opencode:ro" \
#       -v dashd-data:/var/lib/dashd \
#       -p 127.0.0.1:8753:8753 \
#       ghcr.io/tonym128/dashd \
#       serve-and-scan -addr 0.0.0.0:8753 \
#         -auth-token "$(openssl rand -hex 32)" \
#         -models /usr/local/lib/dashd/models.json \
#         -db /var/lib/dashd/dashboard.db
#
# The trailing command is required, and its absence is the point. The image
# binds 127.0.0.1 inside the container by default, so `-p` on its own reaches
# nothing; publishing the dashboard takes an explicit non-loopback bind and a
# token, together, every time. See the long note at the bottom of this file.
#
# Repeating -models and -db is not ceremony: passing any command replaces CMD
# wholesale, so those two flags go with it. -db in particular decides where the
# database lives — the named volume above, or $HOME — and changing that by
# accident is the kind of thing that looks fine until a container is replaced.
#
# Two traps, both hit and fixed while writing this:
#
#   * The writable HOME is not optional. dashd resolves a default database path
#     under $HOME before it reads -db, and os.UserHomeDir() turns into an error
#     when HOME is not a writable directory. A read-only /home/dashd fails with
#     "mkdir /home/dashd/.local: permission denied".
#   * A named volume is created owned by the image's uid 10001, so a
#     host-uid-mapped container cannot write it ("unable to open database
#     file (14)"). Pre-create the volume as your own uid:
#         docker volume create dashd-data && sudo chown "$(id -u)" dashd-data
#     or just bind-mount a directory you own instead of using a named volume.
#
# Bind-mounting the specific agent directories read-only is the better habit:
# it grants the narrowest possible access, and `:ro` means a bug in dashd
# cannot rewrite or delete an agent's history. If your logs are already
# group- or world-readable, the fixed uid 10001 below works with no --user at
# all — but then dashd will report zero activity for anything it cannot read.
# ---------------------------------------------------------------------------
RUN addgroup -g 10001 -S dashd \
 && adduser -u 10001 -S -G dashd -h /home/dashd -s /sbin/nologin dashd \
 && mkdir -p /home/dashd /var/lib/dashd /usr/local/lib/dashd \
 && chown -R dashd:dashd /home/dashd /var/lib/dashd

# ---------------------------------------------------------------------------
# The price dump is BAKED IN, and this is the important part.
#
# dashd carries a small price table compiled into the binary, so a missing
# models.json does not crash it and does not look obviously broken. It prices
# the ~41 models in that fallback table and silently reports $0.00 for
# everything else, on a dashboard whose entire job is money. So the dump is
# copied into the image at build time.
#
# It is baked rather than fetched during the build on purpose: the build stays
# hermetic and reproducible, and the price data that shipped is the same
# reviewed data that is in git. Refresh it deliberately with
# `python3 update_models.py && docker build .`, never implicitly.
#
# LOCATION MATTERS, and this is not cosmetic. defaultModelsPath() looks for
# models.json beside the executable and one directory above it, so the dump is
# colocated with the binary in /usr/local/lib/dashd/. That works however the
# process is invoked, including `docker run img scan`, which REPLACES CMD and
# so drops any -models flag baked into CMD. Measured: with the dump reachable
# only through -models, an overridden CMD fell back to the embedded table and
# priced x-ai/grok-build-0.1 at $0.00 where the catalogue prices it at $3.00.
#
# -models is still passed in CMD as a second, explicit route, but it is the
# colocation that is load-bearing.
# ---------------------------------------------------------------------------
COPY --from=build /out/dashd /usr/local/lib/dashd/dashd
COPY models.json /usr/local/lib/dashd/models.json
RUN chmod 0755 /usr/local/lib/dashd/dashd

USER dashd

# So `dashd` works as a command name inside the container, not just via the
# absolute ENTRYPOINT path.
ENV PATH="/usr/local/lib/dashd:${PATH}"
ENV HOME=/home/dashd

# The SQLite database. Named volume so it survives `docker run` churn; the DB
# is the only state dashd owns.
VOLUME ["/var/lib/dashd"]

# EXPOSE is documentation, not a firewall: it publishes nothing on its own. It
# is here because the healthcheck and the usual invocations both talk about
# 8753, and a port you cannot name is a port you have to count.
#
# Note that the dashboard has no authentication unless -auth-token is given.
EXPOSE 8753

# getenv/wget come from busybox, which is already in Alpine. /healthz returns
# stored counts and the last scan time, so a non-200 means either the server is
# down or it never finished starting. It is unauthenticated on purpose — see the
# note in internal/web/auth.go about secrets in healthcheck commands — and it
# returns counts only, nothing about what is being worked on.
#
# The address here must match -addr below. With the loopback default it does.
HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD wget --quiet --spider "http://127.0.0.1:8753/healthz" || exit 1

# ---------------------------------------------------------------------------
# The default binds 127.0.0.1 INSIDE the container, which is deliberate.
#
# The image previously defaulted to -addr 0.0.0.0:8753 so that the reflexively
# typed
#
#     docker run -p 8753:8753 <image>
#
# would work. It also meant that exact command published every project path,
# session title, model name and dollar figure on the network with no secret at
# all, and the only thing standing between it and the open internet was a
# startup log line. A default that publishes secrets because the documentation
# said to is the wrong default; the mistake should be a container nothing can
# reach.
#
# So the container now behaves like the binary does: loopback until told
# otherwise. Because Docker's port publishing forwards to the container's
# external interface rather than its loopback, `-p` alone now yields an
# unreachable dashboard. Publishing is a deliberate two-part opt-in — an
# explicit bind AND a token — which is the same rule the binary documents:
#
#     docker run ... <image> serve-and-scan \
#       -addr 0.0.0.0:8753 -auth-token "$(openssl rand -hex 32)"
#
# There is deliberately no ENV DASHD_AUTH_TOKEN here. cmd/dashd reads the token
# from the flag only — it does not consult the environment — so an ENV would be
# a variable that silently does nothing, which is worse than no variable at all.
# Support for DASHD_AUTH_TOKEN is a follow-up in cmd/, and it belongs there:
#
#     TODO: read DASHD_AUTH_TOKEN in cmd/dashd/cli.go, then add
#       ENV DASHD_AUTH_TOKEN="" here with a comment saying the default is empty.
#
# With no command given, the container serves on its own loopback. That is still
# useful: `docker run img scan` and `docker run img stats` are unaffected, and
# the healthcheck above works either way.
#
# Note that this CMD only applies when no command is given. Any command you pass
# replaces it wholesale, flags included, so an invocation that overrides CMD must
# repeat -models and -db. See the verified invocation at the top of this file.
ENTRYPOINT ["/usr/local/lib/dashd/dashd"]
CMD ["serve-and-scan", \
     "-addr", "127.0.0.1:8753", \
     "-models", "/usr/local/lib/dashd/models.json", \
     "-db", "/var/lib/dashd/dashboard.db"]