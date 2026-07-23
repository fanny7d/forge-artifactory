FROM golang:1.26.2-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/artifact-repository \
    ./cmd/artifact-repository

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S -g 65532 artifact \
    && adduser -S -D -H -u 65532 -G artifact artifact \
    && mkdir -p /app/keys /var/lib/artifact-repository \
    && chown -R 65532:65532 /app /var/lib/artifact-repository

COPY --from=build /out/artifact-repository /app/artifact-repository

WORKDIR /app
USER 65532:65532
EXPOSE 8080 8081

ENTRYPOINT ["/app/artifact-repository"]
CMD ["api"]
