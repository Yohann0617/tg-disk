FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY . ./

RUN go build -o app .

FROM alpine:latest

RUN apk add --no-cache ca-certificates openssl tzdata && \
        ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
        && echo "Asia/Shanghai" > /etc/timezone \
        && apk del

WORKDIR /app

COPY --from=builder /app/app .

EXPOSE 8080

CMD ["./app"]
