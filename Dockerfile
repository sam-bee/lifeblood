FROM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /lifeblood ./cmd/lifeblood

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /lifeblood /lifeblood
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/lifeblood"]
CMD ["serve"]
