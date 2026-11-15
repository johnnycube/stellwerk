FROM golang:1.27-trixie@sha256:c47c3fda018d13d43a6bf5ebac3e5bba8a23a35782aa4faf531afaf47c64c39f AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# goolm = pure Go Olm/Megolm, no libolm needed. CGO stays on for SQLite.
ARG VERSION=dev
# add "noha" to build without high-availability code: --build-arg TAGS="goolm noha"
ARG TAGS=goolm
RUN CGO_ENABLED=1 go build -tags "${TAGS}" -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /stellwerk ./cmd/stellwerk

FROM gcr.io/distroless/base-debian13:nonroot@sha256:a08d9054afbaf1af9b90fd02cdc7b32236319b5376d374505a41548d42b0c5db
COPY --from=build /stellwerk /stellwerk
VOLUME /data
EXPOSE 8080 8081
USER nonroot
# Config: mount a file and set STELLWERK_CONFIG, mount Secrets/ConfigMaps and
# set STELLWERK_CONFIG_DIR, or pass STELLWERK_* variables. See README.
ENTRYPOINT ["/stellwerk"]
