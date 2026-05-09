FROM golang:1.1.26.2-alpine AS builder

RUN apk add --no-cache git

WORKDIR /tmp/build

COPY go.mod .
COPY go.sum .

RUN go mod download

COPY . .

RUN go build -o ./out/app-srv ./cmd/server/main.go
RUN go build -o ./out/app-cli ./cmd/cli/main.go

FROM alpine:3.20

WORKDIR /app

COPY --from=builder /tmp/build/out/app-srv /app/app-srv
COPY --from=builder /tmp/build/out/app-cli /app/app-cli
COPY --from=builder /tmp/build/.env /app/.env
COPY --from=builder /tmp/build/docs /app/docs

EXPOSE 8080

CMD ["/app/app-srv"]
