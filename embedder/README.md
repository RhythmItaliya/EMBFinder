# Manual Setup: AI Image Embedder

This service handles visual vectorization using CLIP.

## Setup & Run
1. **Install Python**: Ensure Python 3.9+ is installed.
2. **Setup Virtual Environment**:
   ```bash
   python3 -m venv .venv
   source .venv/bin/activate
   ```
3. **Install Dependencies**:
   ```bash
   pip install -r requirements.txt
   ```
4. **Run Service**:
   ```bash
   uvicorn main:app --port 8766
   ```

> [!NOTE]
> Run this command from the `embedder/` folder. The first run downloads the CLIP model and can take a few minutes.

> [!TIP]
> The Go backend can automatically start this service as a subprocess if the files are in the expected directory.
