FROM golang:1.25-alpine AS builder
WORKDIR /build
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o hytale-auth .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /build/hytale-auth .
EXPOSE 3002
CMD ["./hytale-auth"]