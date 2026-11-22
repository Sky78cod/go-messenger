# Go 1.25+ is required by pgx v5.13.0 and x/crypto v0.55.0.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/messenger ./cmd/server

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S messenger && adduser -S messenger -G messenger
WORKDIR /app
COPY --from=build /out/messenger /app/messenger
COPY web/ /app/web/
USER messenger
EXPOSE 8080
CMD ["/app/messenger"]
