FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
ENV WINEPREFIX=/root/.wine-emb-engine
ENV WINEARCH=win64
ENV DISPLAY=:99

# Install Wine + Python
RUN dpkg --add-architecture i386 \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        wine wine32 wine64 winetricks xvfb cabextract \
        python3 python3-pip curl \
    && rm -rf /var/lib/apt/lists/*

# Init Wine prefix + VC++ runtimes for modern engines (Wilcom 2025+)
RUN Xvfb :99 -screen 0 1024x768x24 & sleep 2 \
    && DISPLAY=:99 wineboot --init \
    && sleep 3 \
    && DISPLAY=:99 winetricks -q vcrun2022 vcrun2019 vcrun2015 \
    && wineserver -k || true

# Install EmbEngine ES e4.2
# ── HOW TO ENABLE ────────────────────────────────────────────────────────────
# 1. Place your EmbEngine ES e4.2 installer in this folder (emb-engine/)
# 2. Set build arg: docker compose build --build-arg ES_INSTALLER=setup.exe
# 3. Rebuild
ARG ES_INSTALLER=
ARG SKIP_ES=1

COPY . /tmp/ctx/
RUN if [ "$SKIP_ES" != "1" ] && [ -n "$ES_INSTALLER" ] && [ -f "/tmp/ctx/$ES_INSTALLER" ]; then \
        echo "[EmbEngine] Starting Xvfb for installation..." \
        && (Xvfb :99 -screen 0 1024x768x24 &) \
        && sleep 5 \
        && echo "[EmbEngine] Running installer: $ES_INSTALLER /S" \
        && DISPLAY=:99 wine "/tmp/ctx/$ES_INSTALLER" /S \
        && echo "[EmbEngine] Waiting for installer to complete (this can take several minutes)..." \
        && sleep 60 \
        && while pgrep -f ".exe" > /dev/null; do echo "[EmbEngine] Installer still running..." && sleep 30; done \
        && wineserver -k || true \
        && echo "[EmbEngine] Installation process finished."; \
    else \
        echo "[EmbEngine] SKIPPING ES installation (SKIP_ES=$SKIP_ES, installer=$ES_INSTALLER)."; \
    fi \
    && rm -rf /tmp/ctx

# Python Flask server
WORKDIR /app
RUN pip3 install flask pillow --no-cache-dir --break-system-packages
COPY server.py /app/server.py

EXPOSE 8767

# Start Xvfb then Flask
CMD bash -c "Xvfb :99 -screen 0 1024x768x24 &>/dev/null & sleep 1 && python3 server.py"
