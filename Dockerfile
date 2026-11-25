FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pic-tg .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates libjpeg-turbo-utils libwebp-tools \
    && adduser -D -H -u 10001 pictg \
    && mkdir -p /data \
    && chown pictg:pictg /data

WORKDIR /app
COPY --from=builder /out/pic-tg /usr/local/bin/pic-tg

USER pictg
EXPOSE 8798
VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/pic-tg", "-config", "/data/config.json"]
