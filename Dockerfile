FROM golang:1.14-alpine3.11 as builder
RUN mkdir -p /go/src/github.com/mendersoftware/integration-test-runner
WORKDIR /go/src/github.com/mendersoftware/integration-test-runner
ADD ./ .
RUN CGO_ENABLED=0 go build

FROM alpine:3.11
EXPOSE 8083
RUN apk add git
RUN git clone https://github.com/mendersoftware/integration.git /integration
ENV INTEGRATION_DIRECTORY="/integration/"
ENV GIN_RELEASE=release
ENV INTEGRATION_TEST_RUNNER_LOG_LEVEL=debug
COPY --from=builder /go/src/github.com/mendersoftware/integration-test-runner/integration-test-runner /
ENTRYPOINT ["/integration-test-runner"]
