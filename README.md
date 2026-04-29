# EMBFind - Embroidery Visual Search

EMBFind is a high performance, local first visual search engine for embroidery files and images. It uses a standalone Go server for lightning-fast orchestration and a Python ViT-L-14 AI model for maximum precision vector search.

---

## Quick Start (Docker)

The fastest way to get EMBFinder running is via Docker. This handles all dependencies (Go, Python, Wine) for you.

### 1. Extract Wilcom Installer
If you plan to use the Wilcom Engine, extract your `1-Setup` folder into the `emb-engine/` directory.
- **Required**: `emb-engine/1.exe`

### 2. Configure Environment
Update the `EMB_LIB` path in your `.env` file to point to your embroidery collection.

### 3. Run with Docker Compose
```bash
docker-compose up --build
```
*Note: The first build will take several minutes to install Wine and the Wilcom software.*

---

## Manual Developer Setup

If you prefer to run components natively on your machine, follow the individual setup guides in each directory:

- [**Go Backend Orchestrator**](./go-server/README.md)
- [**AI Image Embedder**](./embedder/README.md)
- [**Wilcom Embroidery Engine**](./emb-engine/README.md)
- [**Electron UI Shell**](./electron/README.md)

---

### Disclaimer
This software is capable of interfacing with native third-party embroidery digitizing engines. We do not distribute or provide licenses for any third-party proprietary software. It is your responsibility to ensure you have legal licenses for any engines you connect.
