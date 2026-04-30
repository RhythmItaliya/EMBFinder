# EMBFind - Embroidery Visual Search

EMBFind is a high performance, local first visual search engine for embroidery files and images. It uses a standalone Go server for lightning-fast orchestration and a Python ViT-L-14 AI model for maximum precision vector search.

---

## Quick Start (Docker)

The fastest way to get EMBFinder running is via Docker. This handles all dependencies (Go, Python, Wine) so it runs on any machine without installing Go/Node/Python.

### Prerequisites
- Docker Engine + Docker Compose

### 1. (Optional) Wilcom Installer for .emb Previews
If you plan to use the Wilcom Engine for `.emb` previews, place your installer in `emb-engine/`.
- **Required**: `emb-engine/1.exe`

### 2. Configure Environment
Use a single `.env` at the project root. This file is used by Docker Compose and the Go server (it loads `../.env`).
Update the `EMB_LIB` path in your `.env` file to point to your embroidery collection.
```bash
cp .env.example .env
# edit .env and set EMB_LIB=/path/to/your/embroidery_library
```

### 3. Run with Docker Compose
```bash
docker-compose up --build
```
*Note: The first build can take several minutes (Wine + model downloads).*

### 4. Access the App
- Web UI: http://127.0.0.1:8765
- Desktop UI is available in local (non-Docker) dev mode via Wails

---

## Manual Developer Setup

If you prefer to run components natively on your machine, follow the individual setup guides in each directory or use the Linux quick start below.

### Linux Quick Start (Local Dev)

#### 1) System Dependencies
```bash
sudo apt update
sudo apt install -y build-essential pkg-config \
	python3-venv python3-pip \
	libgtk-3-dev libwebkit2gtk-4.0-dev \
	libglib2.0-dev libcairo2-dev libpango1.0-dev \
	libgdk-pixbuf2.0-dev libatk1.0-dev \
	libx11-dev libxext-dev libxi-dev libxrandr-dev \
	libxcursor-dev libxdamage-dev \
	xvfb wine64 winetricks
```

#### 2) Start Services (each in its own terminal)

**Embedder (required for image indexing)**
```bash
cd embedder
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
uvicorn main:app --port 8766
```

**Embroidery Engine (optional, only for .emb previews)**
```bash
cd emb-engine
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
python3 server.py
```

**Go Desktop App (Wails)**
```bash
cd go-server
go mod download
go run -tags dev .
```

Open the UI at http://127.0.0.1:8765 (desktop window should also appear).

---

### Manual Setup by Component
If you prefer to run components natively on your machine, follow the individual setup guides in each directory:

- [**Go Backend Orchestrator**](./go-server/README.md)
- [**AI Image Embedder**](./embedder/README.md)
- [**Wilcom Embroidery Engine**](./emb-engine/README.md)
- [**Electron UI Shell**](./electron/README.md)

---

### Disclaimer
This software is capable of interfacing with native third-party embroidery digitizing engines. We do not distribute or provide licenses for any third-party proprietary software. It is your responsibility to ensure you have legal licenses for any engines you connect.
