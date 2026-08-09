# The two supervised images (ADR 0121).
#
# **Packaging artefacts, not builds.** Both copy the binary the release workflow
# already cross-compiled and compile nothing, so a container deployment and a
# bare-metal one run identical bytes — the "same binary, different topology"
# property ADR 0080 turns on.
#
#   lite  Supervisor + ffmpeg              a homelab that already runs PostgreSQL
#   full  Supervisor + ffmpeg + PostgreSQL one `docker run` and a working Mosaic
#
# **Neither image contains the Platform or the Shell**, and that is the whole
# shape of the supervised install rather than an omission: the Supervisor fetches
# a signed Generation on first boot, verifies it and activates it, and the
# recovery page shows that happening to anybody who opens the URL while it does.
# An image carrying them would pin two versions to the image tag and make an
# upgrade a re-pull.
#
# ffmpeg is in **both**, and that is not an optimisation. The Platform shells out
# to ffprobe to decide what a release is and to ffmpeg to re-encode what a client
# cannot decode (ADR 0050); absent, it relays unprobed and a release with
# undecodable audio plays silently. An image that omitted it would be a subtly
# broken Mosaic whose breakage presents as bad media rather than as an error.
#
# debian-slim rather than distroless or scratch for the same reason the
# Platform's image is: a scratch image cannot carry ffmpeg, and the saving is not
# worth shipping a Mosaic that cannot probe.

# ---------------------------------------------------------------- base --------
FROM debian:bookworm-slim AS base

# TARGETARCH is set per platform by buildx, so one Dockerfile produces both the
# amd64 and arm64 image from the matching pre-built binary.
ARG TARGETARCH

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ffmpeg ca-certificates; \
    rm -rf /var/lib/apt/lists/*; \
    ffprobe -version | head -n 1

# The binary arrives executable from `go build` and COPY preserves its mode, so
# no chmod is needed — and avoiding COPY --chmod keeps the image buildable with a
# plain `docker build`, not only under BuildKit.
COPY dist/mosaic-supervisor-linux-${TARGETARCH} /usr/local/bin/mosaic-supervisor

# /var/lib/mosaic holds the Generations and the pointer at the live one, and must
# survive a restart or every boot would be a first boot. /run/mosaic holds the
# children's sockets (ADR 0120) and must not.
VOLUME ["/var/lib/mosaic"]

# **The working directory is inherited by the children, and it is load-bearing.**
# A child inherits the Supervisor's, and the Platform resolves several paths
# relative to it — its telemetry log directory, and the extension install
# directory (ADR 0081). Left at `/` the Platform cannot create either: it exits
# non-zero, the Supervisor restarts it to its ceiling, the activation reverts,
# and a first boot fails with "exit status 1" and nothing about a directory. It
# is set on the base stage so both images have it rather than one.
WORKDIR /var/lib/mosaic

# 8443 is the one public port. There is deliberately no second: ADR 0005 makes
# the front door the only public entry point, and the children listen on Unix
# sockets inside the container rather than on addresses anything could reach.
EXPOSE 8443

# ---------------------------------------------------------------- lite --------
# For a homelab that already runs PostgreSQL. It is the smaller image and the
# less opinionated one: MOSAIC_POSTGRES_DSN points at somebody's own database,
# and nothing here has an opinion about it.
FROM base AS lite

# A non-root user: the Supervisor needs no privilege to serve. It does exec its
# children, which inherit this — so this is also what the Platform and the Shell
# run as.
RUN useradd --system --uid 10001 --home /var/lib/mosaic mosaic; \
    mkdir -p /var/lib/mosaic /run/mosaic; \
    chown mosaic:mosaic /var/lib/mosaic /run/mosaic

USER mosaic
ENTRYPOINT ["/usr/local/bin/mosaic-supervisor"]

# ---------------------------------------------------------------- full --------
# One `docker run` and a working Mosaic. The default, and what the documentation
# shows first.
#
# **PostgreSQL is started and stopped by the entrypoint, not by the Supervisor**,
# and the reason is worth stating because it looks like the wrong way round in
# the process manager's own image.
#
# The Supervisor's child model probes over HTTP — a readiness path and a serving
# path on a listener — and PostgreSQL answers neither. Making it a child would
# mean a second kind of probe and a start ordering the model does not have today,
# which is a change to how every child works in order to package one of them. It
# is also the odd case rather than the general one: in `lite` and on the DIY path
# the database is outside the container entirely, so the Supervisor manages it in
# one deployment of three.
#
# What the entrypoint must get right is the *ordering*, and it does: the database
# comes up before the Supervisor and goes down after it, so the Platform is
# already stopped when the database stops. Docker stopping the two independently
# is what would cost a recovery on the next boot.
FROM base AS full

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends postgresql postgresql-client; \
    rm -rf /var/lib/apt/lists/*

COPY docker-entrypoint-full.sh /usr/local/bin/mosaic-entrypoint
RUN chmod 0755 /usr/local/bin/mosaic-entrypoint

# Root, unlike `lite`, because the entrypoint starts PostgreSQL as the postgres
# user and drops to mosaic for the Supervisor. Nothing in this image runs as root
# beyond the entrypoint's own setup.
RUN useradd --system --uid 10001 --home /var/lib/mosaic mosaic

VOLUME ["/var/lib/postgresql"]
ENTRYPOINT ["/usr/local/bin/mosaic-entrypoint"]
