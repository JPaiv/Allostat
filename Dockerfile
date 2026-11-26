# Build the coordinator.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY admission admission
COPY api api
COPY cmd cmd
COPY internal internal
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /allostat ./cmd/allostat

# The image holds the one binary and nothing to log in to.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /allostat /allostat
USER 65532:65532
ENTRYPOINT ["/allostat"]
