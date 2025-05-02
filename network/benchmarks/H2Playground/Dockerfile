# Build image
FROM golang:1.23 AS build-env

ARG gopkg=k8s.io/perf-tests/network/benchmarks/H2Playground
ADD ["./", "/go/src/${gopkg}"]
WORKDIR /go/src/${gopkg}
RUN ls -la

RUN CGO_ENABLED=0 go build -o /h2playground ./cmd/h2server/main.go

# Runtime image
FROM alpine:3.20
# install curl
RUN apk add --no-cache curl

# install tcpdump
RUN apk add --no-cache tcpdump

COPY --from=build-env /h2playground /h2playground

#Make sure that when compiled on debian, the binary will run on alpine
RUN mkdir -p /lib64 && ln -s /lib/ld-musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2

ENTRYPOINT ["/h2playground"]