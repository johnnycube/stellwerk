FROM golang:1.27-trixie@sha256:430985542100b871e108ccc00e8dcc5eef08ca14ee5825e98ebf563d8ffec4ee AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# goolm = pure Go Olm/Megolm, no libolm needed. CGO stays on for SQLite.
ARG VERSION=dev
# add "noha" to build without high-availability code: --build-arg TAGS="goolm noha"
ARG TAGS=goolm
RUN CGO_ENABLED=1 go build -tags "${TAGS}" -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /stellwerk ./cmd/stellwerk

FROM gcr.io/distroless/base-debian13:nonroot@sha256:7088bf1cdc94a65af4e0d5ad94518ed489a372dec1ece38ff4411c6c15fb5864
COPY --from=build /stellwerk /stellwerk
VOLUME /data
EXPOSE 8080 8081
USER nonroot
# Config: mount a file and set STELLWERK_CONFIG, mount Secrets/ConfigMaps and
# set STELLWERK_CONFIG_DIR, or pass STELLWERK_* variables. See README.
ENTRYPOINT ["/stellwerk"]
