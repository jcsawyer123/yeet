FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/yeet ./cmd/yeet

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/yeet /yeet
EXPOSE 7000
ENTRYPOINT ["/yeet"]
