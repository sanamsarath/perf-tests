FROM golang:1.23 AS build-env
ARG gopkg=k8s.io/perf-tests/util-images/network/netperfbenchmark

ADD [".", "/go/src/$gopkg"]

#ENV GO111MODULE on
WORKDIR /go/src/$gopkg
RUN CGO_ENABLED=0 go build -o /workspace/network ./cmd

FROM  golang:1.23-alpine3.21
ENV LD_LIBRARY_PATH=/usr/local/lib

RUN apk update \
    && apk add curl wget net-tools gcc g++ make \
    && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /tmp

RUN curl -LO https://iperf.fr/download/source/iperf-2.0.9-source.tar.gz && tar zxf iperf-2.0.9-source.tar.gz
RUN rm -rf iperf-2.0.9-source.tar.gz
RUN cd iperf-2.0.9 && ./configure --prefix=/usr/local --bindir /usr/local/bin && make && make install

ENV SIEGE_VERSION=4.1.7

RUN curl -LO http://download.joedog.org/siege/siege-$SIEGE_VERSION.tar.gz > siege-$SIEGE_VERSION.tar.gz && tar -xf siege-${SIEGE_VERSION}.tar.gz
RUN rm -rf siege-${SIEGE_VERSION}.tar.gz
RUN cd siege-${SIEGE_VERSION} && ./configure && make && make install

WORKDIR /workspace
COPY --from=build-env /workspace/network .
ENTRYPOINT ["/workspace/network"]
