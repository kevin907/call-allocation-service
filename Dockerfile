FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies resolve from go.mod alone, so this layer is not invalidated by a
# source edit. There are none today, which is the point.
COPY go.mod ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# A static binary is what lets the final stage carry no distribution at all.
ENV CGO_ENABLED=0
ARG TARGETOS TARGETARCH
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/callallocator ./cmd/callallocator

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/callallocator /callallocator

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/callallocator"]
