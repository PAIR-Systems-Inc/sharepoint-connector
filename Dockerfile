FROM python:3.11-slim
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY listener.py sharepoint_client.py goodmem_client.py .
EXPOSE 8080
ENV PORT=8080
CMD ["python", "listener.py", "server"]
