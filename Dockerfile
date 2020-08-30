FROM golang:1.15 AS build
WORKDIR /alertmanager-status

COPY go.mod go.sum /alertmanager-status/
RUN go mod download

COPY . /alertmanager-status/
RUN CGO_ENABLED=0 go install .

FROM gcr.io/distroless/static-debian10
WORKDIR /
COPY --from=build /go/bin/alertmanager-status /go/bin/alertmanager-status
CMD ["/go/bin/alertmanager-status"]
