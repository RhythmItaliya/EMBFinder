# ── Build ─────────────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /embfinder .

# ── Run ───────────────────────────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
COPY --from=build /embfinder /embfinder
EXPOSE 8765
ENV EMBFIND_DATA_DIR=/data
ENTRYPOINT ["/embfinder"]
