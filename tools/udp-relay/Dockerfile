ARG GO_VERSION=1.26.5
ARG GO_IMAGE_VARIANT=bookworm

FROM golang:${GO_VERSION}-${GO_IMAGE_VARIANT}

ARG GO_VERSION

WORKDIR /app
COPY main.go .

RUN actual="$(go env GOVERSION)" \
    && test "$actual" = "go${GO_VERSION}" \
       || (echo "ERROR: required go${GO_VERSION}, actual=${actual}" && exit 1)
RUN go build -o /usr/local/bin/udp-relay main.go

ENTRYPOINT ["udp-relay"]
