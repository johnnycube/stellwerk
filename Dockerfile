FROM golang:1.27-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# goolm = pure Go Olm/Megolm, no libolm needed. CGO stays on for SQLite.
ARG VERSION=dev
# add "noha" to build without high-availability code: --build-arg TAGS="goolm noha"
ARG TAGS=goolm
RUN CGO_ENABLED=1 go build -tags "${TAGS}" -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /stellwerk ./cmd/stellwerk

FROM gcr.io/distroless/base-debian13:nonroot
COPY --from=build /stellwerk /stellwerk
VOLUME /data
EXPOSE 8080 8081
USER nonroot
# Config: mount a file and set STELLWERK_CONFIG, mount Secrets/ConfigMaps and
# set STELLWERK_CONFIG_DIR, or pass STELLWERK_* variables. See README.
ENTRYPOINT ["/stellwerk"]
