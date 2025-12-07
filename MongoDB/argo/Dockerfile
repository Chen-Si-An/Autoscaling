FROM alpine/helm:latest

USER root
RUN apk add --no-cache curl
RUN curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
    && install -o root -g root -m 0755 kubectl /usr/local/bin/kubectl \
    && rm kubectl

USER 1001
WORKDIR /work
ENV HELM_CACHE_HOME=/tmp/.cache
ENV HELM_CONFIG_HOME=/tmp/.config
CMD ["helm", "version"]