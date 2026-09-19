# syntax=docker/dockerfile:1
FROM golang:alpine AS build
RUN apk --no-cache add build-base vips-dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -buildvcs=false -trimpath -ldflags='-s -w' -o /ngax .

FROM alpine:3.20
RUN apk --no-cache add vips ca-certificates tzdata \
    && addgroup -S ngax && adduser -S -G ngax ngax
WORKDIR /app
COPY --from=build /ngax /app/ngax
USER ngax
ENV VIPS_CONCURRENCY=1 \
    MALLOC_ARENA_MAX=2
EXPOSE 8080 9080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/health || exit 1
CMD ["/app/ngax"]
