FROM python:3.11-slim

WORKDIR /app
COPY requirements.txt .
RUN apt-get update \
	&& apt-get install -y --no-install-recommends curl \
	&& rm -rf /var/lib/apt/lists/* \
	&& pip install --no-cache-dir --default-timeout=1000 -r requirements.txt

COPY main.py .
EXPOSE 8766
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8766"]
