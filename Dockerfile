FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg \
    ca-certificates \
    curl \
    tar \
    && rm -rf /var/lib/apt/lists/*

RUN curl -L https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp -o /usr/local/bin/yt-dlp \
    && chmod a+rx /usr/local/bin/yt-dlp

WORKDIR /app

RUN mkdir -p /app/bin

RUN curl -fsSL https://github.com/thruqelabs/whatsapp-go/releases/download/alpha/whatsrook-linux-amd64.tar.gz -o /tmp/whatsrook.tar.gz \
    && tar -xzf /tmp/whatsrook.tar.gz -C /app/bin \
    && rm -f /tmp/whatsrook.tar.gz \
    && chmod +x /app/bin/whatsrook

ENV PATH="/app/bin:${PATH}"

WORKDIR /app/bin

ENTRYPOINT ["whatsrook"]