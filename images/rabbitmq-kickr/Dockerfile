# syntax=docker/dockerfile:1
#
# rabbitmq-kickr wraps bitnamilegacy/rabbitmq's real container image with
# kickr (github.com/bytepunx/kickr) as its entrypoint, per authstar's ADR
# 0010 (~/git/authstar/design/decisions/0010-kickr-for-rabbitmq-container.md).
# kickr fetches RabbitMQ's bootstrap credential (and anything else
# env-var-configurable) from signet at container start, injects it into the
# environment, execs the real RabbitMQ entrypoint, and coordinates a
# fleet-wide serialized restart when the bundle changes.
#
# Base image: bitnamilegacy/rabbitmq, not bitnami/rabbitmq — Broadcom's 2025
# restructuring of Bitnami's Docker Hub distribution removed this pinned tag
# from the free "bitnami/*" org (confirmed via `docker manifest inspect
# docker.io/bitnami/rabbitmq:4.1.3-debian-12-r1` returning "not found"); the
# same tag is still served from the free-but-frozen "bitnamilegacy/*" org.
# This must be kept in sync with kluster-lib/addon/rabbitmq.go's own
# rabbitmqBaseImageTag constant.
ARG RABBITMQ_BASE_TAG=4.1.3-debian-12-r1
ARG KICKR_VERSION=v1.0.0

# kickr ships only as a distroless, no-shell standalone image
# (bytepunx/kickr on Docker Hub) — never as a library — so "running X under
# kickr" always means building a new composite image that copies kickr's
# static binary in and replaces the entrypoint. See ~/git/kickr/design/draft.md
# section 10 for kickr's own distribution model.
#
# A named stage (rather than `COPY --from=docker.io/bytepunx/kickr:${KICKR_VERSION}`
# directly) is required here: BuildKit doesn't expand ARGs inside --from
# image references, only inside FROM.
FROM docker.io/bytepunx/kickr:${KICKR_VERSION} AS kickr

FROM docker.io/bitnamilegacy/rabbitmq:${RABBITMQ_BASE_TAG}

COPY --from=kickr /kickr /usr/local/bin/kickr

# Inspecting the base image directly (`docker inspect ... .Config.Entrypoint
# / .Config.Cmd`) shows RabbitMQ's real startup is
# "/opt/bitnami/scripts/rabbitmq/entrypoint.sh /opt/bitnami/scripts/rabbitmq/run.sh".
# entrypoint.sh is NOT a thin wrapper: it copies default config into place
# and, specifically when its argv is run.sh, runs setup.sh (which reads
# RABBITMQ_USERNAME/RABBITMQ_PASSWORD and creates/updates that user — see
# /opt/bitnami/scripts/rabbitmq/setup.sh and librabbitmq.sh). Pointing
# KICKR_COMMAND straight at run.sh, skipping entrypoint.sh, would silently
# skip that setup step and defeat the entire point of injecting a
# signet-sourced credential. So KICKR_COMMAND wraps entrypoint.sh itself,
# with KICKR_ARGS reproducing its original argv (run.sh) — kickr then
# execs exactly "entrypoint.sh run.sh", identical to the base image's own
# unmodified behaviour, just with kickr as PID 1 in front of it.
#
# Note bitnami's own env var names are RABBITMQ_USERNAME / RABBITMQ_PASSWORD
# (confirmed via librabbitmq.sh), not the upstream rabbitmq:* image's
# RABBITMQ_DEFAULT_USER / RABBITMQ_DEFAULT_PASS — easy to get wrong since
# ADR 0010 references the latter by analogy only.
ENV KICKR_COMMAND=/opt/bitnami/scripts/rabbitmq/entrypoint.sh
ENV KICKR_ARGS=/opt/bitnami/scripts/rabbitmq/run.sh

# The spiffe-csi-driver mounts the SPIRE Workload API socket as
# "spire-agent.sock", not kickr's own built-in default filename
# ("agent.sock") — see kluster-lib/addon/probe.go's svProbeSocketPath and
# addon/signet.go's signetSpireSocketPath, both of which hardcode the same
# non-default filename for the same reason. rabbitmq.go mounts the CSI
# volume at /run/spire/sockets, so this must match.
ENV KICKR_SPIFFE_SOCKET=/run/spire/sockets/spire-agent.sock

# namespace/service have no env var equivalent — kickr requires them via a
# .kickr.yaml file (see ~/git/kickr/design/draft.md section 3.2) — so
# rabbitmq.go mounts a ConfigMap containing that file as a volume and this
# just needs to point at its fixed, non-cluster-specific mount path.
# (KICKR_CONFIG_PATH itself has an env var, unlike namespace/service, but
# the *path* it points to is baked into both this image and rabbitmq.go's
# extraVolumeMounts identically, so there's nothing cluster-specific to
# inject here — it belongs in the image, not the Helm values.)
ENV KICKR_CONFIG_PATH=/etc/kickr/.kickr.yaml

# KICKR_SIGNET_ADDR / KICKR_SIGNET_SPIFFE_ID are cluster-specific (the
# trust domain varies per kluster invocation), so those are supplied at
# deploy time via the Helm chart's extraEnvVars (see rabbitmq.go), not
# baked in here.

# Base image's own USER (1001, non-root) is inherited unchanged; kickr's
# binary is copied in as root during build but only needs to be
# world-executable, which COPY preserves from the source image layer.
ENTRYPOINT ["/usr/local/bin/kickr"]
