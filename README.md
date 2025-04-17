# Constructo - AI Terminal Agent

Constructo is an advanced AI terminal agent designed for versatility and deep system interaction.

## Features

- Deep Reasoning
- Persistent Memory
- AI Judge for self-evaluation
- Advanced Terminal Control
- Cross-platform compatibility

## Getting Started

### Prerequisites

- Go (version 1.22 or later)
- Docker (for containerized execution)
- A Gemini API Key (from Google AI Studio)

### Installation & Running

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/rafabd1/Constructo.git
    cd Constructo
    ```

2.  **Configure API Key:**
    - Copy the example configuration: `cp configs/config.yaml.example config.yaml`
    - Edit `config.yaml` and paste your Gemini API Key into the `llm.api_key` field.
    - **Important:** Ensure `config.yaml` is in the project root directory and is excluded from Git commits (added to `.gitignore`).

3.  **Build and Run with Docker (Recommended):**
    This method ensures a consistent Linux environment where terminal interaction is most reliable.
    ```bash
    # Build the Docker image
    docker build -t constructo-agent .

    # Run the agent interactively in the container
    # Make sure config.yaml is in the project root directory when building
    docker run -it --rm --name constructo constructo-agent
    ```

4.  **Build and Run Locally (Requires Linux/macOS for reliable PTY):**
    *Note: Direct execution on Windows is currently **not recommended** due to potential PTY/terminal interaction issues.*
    ```bash
    # Build the binary
    go build -o constructo cmd/constructo/main.go

    # Run the agent (ensure config.yaml is in the current directory)
    ./constructo
    ```

### Usage

Once the agent starts, you can interact with it by typing natural language requests or using internal commands prefixed with `/` (e.g., `/help`).

## Project Structure

```
Constructo/
├── cmd/constructo/         # Main application entry point
├── configs/                # Configuration files (config.yaml.example)
├── examples/               # Usage examples (TODO)
├── instructions/           # Agent instruction files (system_prompt.txt)
├── internal/               # Internal application code
│   ├── agent/              # Core agent logic
│   ├── commands/           # Internal command implementations
│   ├── config/             # Configuration loading
│   ├── judge/              # AI Judge system (Future)
│   ├── memory/             # Memory system (Future)
│   ├── platform/           # Platform detection (Less relevant now)
│   ├── reasoning/          # Reasoning engine (Future)
│   └── terminal/           # Terminal interaction (using creack/pty)
├── pkg/                    # Publicly shared code / integrations (Future)
│   └── utils/              # General utilities
├── scripts/                # Utility scripts (Future)
├── .dockerignore           # Docker build ignore file
├── .gitignore              # Git ignore file
├── Dockerfile              # Docker build definition
├── go.mod                  # Go module definition
├── go.sum                  # Go module checksums
└── README.md               # This file
```

## Advanced Memory System (Planned)

## Contributing

(TODO: Add contribution guidelines)

## License

(TODO: Add license - e.g., MIT)
