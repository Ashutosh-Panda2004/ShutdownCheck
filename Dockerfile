# Release image. GoReleaser supplies the already-built static binary, so this
# stage only assembles it — there is no compilation here and nothing to cache.
#
# Distroless static: no shell, no package manager, no libc. A verification tool
# that people run beside production should not bring an attack surface with it.
#
# The conformance image at test/conformance/docker/Dockerfile is separate and
# builds from source; this one must stay in step with .goreleaser.yaml.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG TARGETPLATFORM
COPY $TARGETPLATFORM/shutdowncheck /usr/local/bin/shutdowncheck

# Runs unprivileged. The tool needs no capabilities of its own: it signals
# containers through the mounted Docker socket, not through host privileges.
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/shutdowncheck"]
CMD ["help"]
