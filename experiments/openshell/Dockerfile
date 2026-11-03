FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61
RUN apt-get update && apt-get install -y --no-install-recommends git gh ca-certificates python3 && rm -rf /var/lib/apt/lists/*
RUN useradd --uid 1000 --create-home --shell /bin/bash sandbox && mkdir -p /sandbox && chown sandbox:sandbox /sandbox
USER sandbox
WORKDIR /sandbox
CMD ["/bin/bash", "-l"]
