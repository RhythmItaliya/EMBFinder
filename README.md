# EMBFind — Embroidery Visual Search

EMBFind is a high-performance, local-first visual search engine for embroidery files and images. It uses a standalone Go server for lightning-fast orchestration and a Python ViT-L-14 AI model for maximum precision vector search.

This application is designed to run **100% locally** with no external API dependencies, ensuring complete privacy and offline capability.

---

## Linux Setup & Run Instructions

| Step | Action | Command / Detail |
| :--- | :--- | :--- |
| **1. Prerequisites** | Install Go and Python 3. | `sudo apt update && sudo apt install -y python3 python3-pip golang-go` |
| **2. Python AI Engine** | Install the embedder dependencies. | `pip3 install --break-system-packages open_clip_torch torch torchvision fastapi uvicorn python-multipart Pillow numpy pystitch` |
| **3. Build Project** | Compile the Go binary. | `cd go-server`<br>`go build -o embfind .` |
| **4. Run Application** | Start the compiled binary. | `./embfind` |

*(Note: The Go binary will automatically start the Python AI subprocess in the background for you. On the first run, it may take a minute to download the high-accuracy ViT-L-14 weights to your local machine).*

---

## macOS Setup & Run Instructions

| Step | Action | Command / Detail |
| :--- | :--- | :--- |
| **1. Prerequisites** | Install Go and Python 3. | `brew install python go` *(Requires Homebrew)* |
| **2. Python AI Engine** | Install the embedder dependencies. | `pip3 install open_clip_torch torch torchvision fastapi uvicorn python-multipart Pillow numpy pystitch` |
| **3. Build Project** | Compile the Go binary. | `cd go-server`<br>`go build -o embfind .` |
| **4. Run Application** | Start the compiled binary. | `./embfind` |

*(Note: Ensure your terminal has permission to access your local Drives/Volumes when prompted so the auto-scanner can index your embroidery folders).*

---

## Windows Setup & Run Instructions

| Step | Action | Command / Detail |
| :--- | :--- | :--- |
| **1. Prerequisites** | Download & Install Go and Python 3. | Download from [golang.org](https://go.dev/dl/) and [python.org](https://www.python.org/downloads/).<br>**Important:** Ensure you check "Add Python to PATH" during installation. |
| **2. Python AI Engine** | Install the embedder dependencies. | Open Command Prompt / PowerShell:<br>`pip install open_clip_torch torch torchvision fastapi uvicorn python-multipart Pillow numpy pystitch` |
| **3. Build Project** | Compile the Go binary. | `cd go-server`<br>`go build -o embfind.exe .` |
| **4. Run Application** | Start the compiled binary. | `.\embfind.exe` |

*(Note: The Go binary detects your `A:\` to `Z:\` drives automatically. The Python embedder runs silently as a subprocess attached to the `.exe`).*

---

### Configuration & Memory
- **No `.env` required:** All required configuration (ports, database path) is auto-configured for a local environment natively in `go-server/config.go`. If ports are busy, it will automatically find free ones.
- **Memory Optimization:** The software uses an aggressive internal garbage collection strategy (`MemoryCleanup()`) to instantly free RAM after bulk indexing heavy folders.

---

### Optional `.env` Variables

If you need advanced control over the application (such as running it on a specific network port, changing the database location, or pointing to external AI servers), you can create a `.env` file in the root directory. 

| Environment Variable | Description | Default Value |
| :--- | :--- | :--- |
| `PORT` | The port the main Go Web UI and API runs on. | `8765` (or a random free port) |
| `DB_PATH` | The absolute path to save the SQLite index and embeddings. | `~/.embfind/embfind.db` |
| `EMBEDDER_PORT` | The local port the Python subprocess binds to. | `8766` (or a random free port) |
| `EMBEDDER_URL` | Bypasses the subprocess entirely. Set this if you host the AI engine on another machine. | `http://127.0.0.1:8766` |
| `CLIP_MODEL` | Advanced: The specific vision model the AI engine should load. | `MobileCLIP-B` |
| `CLIP_PRETRAINED` | Advanced: The specific huggingface pretrained weights to pull. | `datacompdr` |
| `EMB_ENGINE_URL` | Advanced: URL for a background Windows third-party automation server. | `http://localhost:8767` |

---

### ⚠️ Disclaimer & Third-Party Engines
**Use at your own risk.** This software is capable of interfacing with native third-party embroidery digitizing engines (e.g., via the `EMB_ENGINE_URL` automation hook) to extract `.emb` thumbnails and geometries. We do not distribute, endorse, or provide licenses for any third-party proprietary software. It is your strict responsibility to ensure you have obtained legal licenses for any third-party engines you connect to this tool.
