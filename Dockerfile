# Build stage
FROM golang:1.26-bookworm AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o main ./cmd/api/main.go

# Run stage
FROM debian:bookworm-slim
WORKDIR /app
COPY --from=builder /app/main .
# Azure Container Apps uses PORT environment variable, usually 8080
EXPOSE 8080
CMD ["./main"]