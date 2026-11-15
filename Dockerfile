# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
# Dependencies first, so the module download layer is cached across code edits.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# Static binaries need no libc; distroless has no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/ledgerline"]
