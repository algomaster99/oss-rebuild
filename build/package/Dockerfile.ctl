FROM golang:1.24-alpine AS binary
ARG DEBUG=false
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="$([ ${DEBUG} = "true" ] || printf '-s -w')" -gcflags="-l=4" ./tools/ctl
FROM alpine
COPY --from=binary /src/ctl /
ENTRYPOINT ["/ctl"]
