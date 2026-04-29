# ── Build ─────────────────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /embfind .

# ── Run ───────────────────────────────────────────────────────────────────────
FROM scratch
COPY --from=build /embfind /embfind
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
EXPOSE 8765
ENV DB_PATH=/data/embfind.db
ENTRYPOINT ["/embfind"]
