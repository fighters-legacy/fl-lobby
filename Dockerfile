# Build a fully static binary, then ship it on nothing. fl-lobby has no dependencies, reads no
# files and needs no shell, so a scratch image is not austerity for its own sake -- there is
# genuinely nothing else for it to contain, and nothing in it to exploit.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/fl-lobby ./cmd/fl-lobby

FROM scratch
COPY --from=build /out/fl-lobby /fl-lobby
# A non-root numeric UID: there is no /etc/passwd in a scratch image to name one.
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/fl-lobby"]
CMD ["-addr", ":8080"]
