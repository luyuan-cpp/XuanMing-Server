FROM golang:1.26

WORKDIR /app
COPY main.go .

RUN go build -o /usr/local/bin/udp-relay main.go

ENTRYPOINT ["udp-relay"]
