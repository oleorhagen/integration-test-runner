FROM golang:1.14-alpine3.11 as builder
RUN mkdir -p /go/src/github.com/mendersoftware/integration-test-runner
WORKDIR /go/src/github.com/mendersoftware/integration-test-runner
ADD ./ .
RUN CGO_ENABLED=0 go build

FROM golang:1.14-alpine3.11
EXPOSE 8080
RUN apk add git openssh python3 py3-pip
RUN pip3 install --upgrade pyyaml
RUN mkdir -p /root/.ssh/ && \
    git config --global user.name "Mender Root" && \
    git config --global user.email root@mender-jenkins.mender.io
RUN git clone https://github.com/mendersoftware/integration.git /integration
ENV INTEGRATION_DIRECTORY="/integration/"
ENV PATH="/integration/extra:${PATH}"
ENV GIN_RELEASE=release
ENV INTEGRATION_TEST_RUNNER_LOG_LEVEL=debug
COPY --from=builder /go/src/github.com/mendersoftware/integration-test-runner/integration-test-runner /
ADD ./entrypoint /
ENTRYPOINT ["/entrypoint"]
