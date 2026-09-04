# Build
FROM golang:alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /hf-ipfs .

# Run
FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /hf-ipfs /usr/local/bin/hf-ipfs

# No ENV overrides: defaults resolve to /root/.hf-ipfs and
# /root/.cache/huggingface/hub. Mount the real host dirs at those exact
# paths (filestore records absolute paths used at ingest time).
EXPOSE 4008
ENTRYPOINT ["hf-ipfs"]
CMD ["daemon"]
