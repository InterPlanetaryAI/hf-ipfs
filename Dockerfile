# Build
FROM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /sf .

# Run
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /sf /usr/local/bin/sf

# No ENV overrides: defaults resolve to /root/.swarmface and
# /root/.cache/huggingface/hub. Mount the real host dirs at those exact
# paths (filestore records absolute paths used at ingest time).
EXPOSE 4008
ENTRYPOINT ["sf"]
CMD ["daemon"]
