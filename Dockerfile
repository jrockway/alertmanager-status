FROM golang:1.19.1 AS build
WORKDIR /alertmanager-status

COPY go.mod go.sum /alertmanager-status/
RUN go mod download

COPY . /alertmanager-status/
ARG version="unversioned-docker-build"
RUN CGO_ENABLED=0 go install -ldflags "-X github.com/jrockway/opinionated-server/server.AppVersion=${version}" .

FROM gcr.io/distroless/static-debian11
WORKDIR /
COPY --from=build /go/bin/alertmanager-status /go/bin/alertmanager-status
ENTRYPOINT ["/go/bin/alertmanager-status"]
