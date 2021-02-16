FROM golang:1.15 as builder

WORKDIR /workspace/helmdiff
COPY . /workspace/helmdiff
RUN go build

# -----------------------------------------------------------------------------

FROM ubuntu:20.04

RUN apt-get update && apt-get install -y git bash curl jq wget

ARG HELM_VERSION="v3.3.4"
ARG HELM_LOCATION="https://get.helm.sh"
ARG HELM_FILENAME="helm-${HELM_VERSION}-linux-amd64.tar.gz"
ARG HELM_SHA256="b664632683c36446deeb85c406871590d879491e3de18978b426769e43a1e82c"
RUN set -x && \
    wget ${HELM_LOCATION}/${HELM_FILENAME} && \
    echo Verifying ${HELM_FILENAME}... && \
    sha256sum ${HELM_FILENAME} | grep -q "${HELM_SHA256}" && \
    echo Extracting ${HELM_FILENAME}... && \
    tar zxvf ${HELM_FILENAME} && mv /linux-amd64/helm /usr/local/bin/ && \
    rm ${HELM_FILENAME} && rm -r /linux-amd64
RUN mkdir -p /root/.local/share/helm/plugins/bin/
COPY --from=builder /workspace/helmdiff/helmdiff /root/.local/share/helm/plugins/helmdiff/bin/helmdiff
COPY --from=builder /workspace/helmdiff/plugin.yaml /root/.local/share/helm/plugins/helmdiff/plugin.yaml

CMD ["/usr/local/bin/helm"]
