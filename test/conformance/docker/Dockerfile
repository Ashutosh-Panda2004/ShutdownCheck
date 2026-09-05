# Conformance server, containerised so the Docker target can be held to the
# same contract as the process target.
#
# Static and scratch-based on purpose: the server must be PID 1 so that
# `docker kill --signal=TERM` reaches it directly. A shell wrapper would take
# PID 1 itself and swallow the signal, which is a real and common shutdown bug
# but not the one each scenario is trying to reproduce.

FROM golang:1.24-alpine@sha256:8bee1901f1e530bfb4a7850aa7a479d17ae3a18beb6e09064ed54cfd245b7191 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./test/conformance/go

FROM scratch
COPY --from=build /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
