from rich.console import Console, RenderableType
from rich.live import Live
from rich.spinner import Spinner
from rich.prompt import Confirm
from rich.panel import Panel
from rich.text import Text
from rich.console import Group
from rich.box import ROUNDED
from datetime import datetime
import time
import json
import os
from pathlib import Path
from rich.layout import Layout
import signal
import sys

class UnifiedTerminal:
    def __init__(self, log_file="logs/agent_history.log"):
        self.console = Console()
        self.log_file = log_file
        self.messages = []
        self.spinner = None
        self.live = None 
        
        # Create logs directory if it doesn't exist
        log_dir = os.path.dirname(self.log_file)
        if log_dir:
            Path(log_dir).mkdir(parents=True, exist_ok=True)
        
        self.log_styles = {
            "INPUT": "bold cyan",
            "EXEC": "bold yellow",
            "OUTPUT": "green",
            "ERROR": "bold red",
            "WARNING": "yellow",
            "SUCCESS": "bold green",
            "INFO": "blue",
            "AGENT": "bold cyan",
            "THINKING": "bold magenta",
            "ANALYZING": "bold blue",
            "DEEP_REASONING": "bold blue",
            "DIM": "dim"
        }
        
        self.interrupt_handler = None
        self.interrupt_count = 0
        self.last_interrupt_time = 0
        self.setup_interrupt_handler()
        
    def setup_interrupt_handler(self):
        """Configure custom SIGINT (Ctrl+C) handler"""
        signal.signal(signal.SIGINT, self._signal_handler)
        
    def _signal_handler(self, signum, frame):
        """
        Internal signal handler that manages Ctrl+C behavior:
        - Single press: Interrupt current action
        - Double press within 1 second: Exit program
        """
        current_time = time.time()
        if current_time - self.last_interrupt_time <= 1:
            # Double Ctrl+C within 1 second - exit program
            self.log("\nExiting program...", "WARNING")
            sys.exit(0)
        else:
            # Single Ctrl+C - interrupt current action
            self.interrupt_count = 1
            self.last_interrupt_time = current_time
            if self.interrupt_handler:
                self.interrupt_handler()
            self.log("\nAction interrupted by user (Ctrl+C)", "WARNING")
        
    def set_interrupt_handler(self, handler):
        """Set the callback for when interruption occurs"""
        self.interrupt_handler = handler
        
    def clear_interrupt_handler(self):
        """Remove the current interrupt handler"""
        self.interrupt_handler = None
        
    def start_processing(self, message="Thinking...", style="THINKING"):
        """Shows a spinner while processing"""
        if self.live:
            self.stop_processing()
            
        self.spinner = Spinner('dots')
        style_color = self.log_styles.get(style)
        
        class SpinnerText:
            def __rich_console__(self, console, options):
                spinner_frame = self.spinner.render(time.time())
                text = Text()
                text.append(spinner_frame)
                text.append(" ")
                text.append(message)
                text.stylize(style_color)
                yield text
                
            def __init__(self, spinner):
                self.spinner = spinner
            
        self.live = Live(
            SpinnerText(self.spinner),
            console=self.console,
            transient=True,
            refresh_per_second=20
        )
        self.live.start()
        
    def start_deep_reasoning(self):
        """Shows Deep Reasoning header with spinner"""
        if self.live:
            self.stop_processing()
        
        self.spinner = Spinner('dots')
        style_color = self.log_styles.get("DEEP_REASONING")
        
        class ReasoningHeader:
            def __rich_console__(self, console, options):
                spinner_frame = self.spinner.render(time.time())
                text = Text()
                text.append(spinner_frame)
                text.append(" Deep Reasoning...")
                text.stylize(style_color)
                yield text
            
            def __init__(self, spinner):
                self.spinner = spinner
        
        self.live = Live(
            ReasoningHeader(self.spinner),
            console=self.console,
            transient=True,
            refresh_per_second=20
        )
        self.live.start()
        
        self._save_interaction_to_file({
            "timestamp": datetime.now().strftime("%H:%M:%S"),
            "type": "DEEP_REASONING",
            "content": "Starting Deep Reasoning analysis"
        })
        
    def log_deep_reasoning_step(self, message: str):
        """Logs a Deep Reasoning step under the header"""
        if self.live:
            # Traduzir mensagens comuns para inglês
            message = message.replace("Analisando com perspectiva", "Analyzing with perspective")
            message = message.replace("Sintetizando análises", "Synthesizing perspectives")
            
            self.console.print(f"  → {message}", style="dim")
            self._save_interaction_to_file({
                "timestamp": datetime.now().strftime("%H:%M:%S"),
                "type": "DEEP_REASONING_STEP",
                "content": message
            })
        
    def start_analysis(self):
        """Shows analyzing spinner"""
        self.start_processing("Analyzing result...", "ANALYZING")
        
    def stop_processing(self):
        """Stops the spinner and clears the line correctly"""
        if self.live:
            self.live.stop()
            self.live = None
        if self.spinner:
            self.spinner = None
        self.clear_line()
        self.console.print()
        
    def _save_interaction_to_file(self, entry: dict):
        """Save interaction log entry to file with proper formatting"""
        try:
            # Add timestamp if not present
            if "timestamp" not in entry:
                entry["timestamp"] = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            
            # Format JSON with indentation for better readability
            formatted_json = json.dumps(entry, indent=2, ensure_ascii=False)
            
            with open(self.log_file, 'a', encoding='utf-8') as f:
                f.write(formatted_json + "\n")
        except Exception as e:
            # If primary log fails, try backup location
            try:
                backup_file = os.path.expanduser("~/constructo_agent.log")
                with open(backup_file, 'a', encoding='utf-8') as f:
                    f.write(json.dumps(entry, indent=2, ensure_ascii=False) + "\n")
            except:
                # If both fail, just print warning (don't affect main flow)
                print(f"\nWarning: Could not write to log files: {str(e)}")
    
    def log(self, message: str, style: str = "INFO", end: str = "\n", show_timestamp: bool = True):
        """Display log message in terminal"""
        try:
            timestamp = datetime.now().strftime("%H:%M:%S")
            style_color = self.log_styles.get(style)
            
            # Se for DIM, não mostrar o estilo no output
            if style == "DIM":
                self.console.print(message, end=end)
                return
            
            # Formatar timestamp e identificador
            timestamp_str = f"[dim]{timestamp}[/dim]" if show_timestamp else ""
            style_id = f"[{style}]" if style else ""
            
            if style_color:
                formatted_message = f"{timestamp_str} {style_id} {message}"
                self.console.print(formatted_message, style=style_color, end=end)
            else:
                self.console.print(f"{timestamp_str} {style_id} {message}", end=end)
            
            # Only log important interactions
            if style in ["EXEC", "OUTPUT", "AGENT", "ERROR"]:
                self._save_interaction_to_file({
                    "timestamp": timestamp,
                    "type": style,
                    "content": message,
                    "raw_response": getattr(self, '_current_raw_response', None)
                })
        except Exception as e:
            print(f"Logging error: {str(e)}")
            print(message)
        
    def clear_line(self):
        """Clears the last line of the terminal without using ANSI codes"""
        print("\r", end="")  # Return cursor to the beginning of the line
        print(" " * self.console.width, end="\r")  # Clear line with spaces
        
    def stop_spinner(self):
        """Stops the spinner and clears the line"""
        if self.spinner:
            self.clear_line()
            self.spinner = None
            
    async def request_confirmation(self, message: str) -> bool:
        """Requests user confirmation in a cleaner way"""
        self.console.print()  # New line to clear formatting
        return Confirm.ask(message, default=False)
            
    def log_agent(self, message: str):
        """Log agent message with full context"""
        timestamp = datetime.now().strftime("%H:%M:%S")
        
        # Display in terminal
        self.clear_line()
        self.console.print(
            f"[dim]{timestamp}[/dim] [{self.log_styles['AGENT']}][Agent][/] {message}"
        )
        
        # Save detailed interaction log
        self._save_interaction_to_file({
            "timestamp": timestamp,
            "type": "AGENT",
            "content": message,
            "raw_response": getattr(self, '_last_raw_response', None),
            "context": {
                "requires_deep_reasoning": getattr(self, '_current_deep_reasoning', False),
                "command_context": getattr(self, '_current_command_context', None),
                "analysis_type": getattr(self, '_current_analysis_type', None)
            }
        })
        
    def log_command(self, command: str, output: str, return_code: int):
        """Log command execution with full details"""
        # Log do comando primeiro
        self.log(f"Executing command: {command}", "EXEC")
        
        # Esperar um momento para melhor visualização
        time.sleep(0.1)
        
        # Se houver saída, escapar caracteres especiais e exibir
        if output:  # Removido .strip() para preservar formatação
            # Escapar caracteres que podem ser interpretados como tags Rich
            safe_output = output.replace('[', '\\[').replace(']', '\\]')
            style = "ERROR" if return_code != 0 else "OUTPUT"
            self.log(safe_output.rstrip(), style)  # Usar rstrip() para remover quebras extras no final
        
        # Sempre adicionar uma linha de espaço após o output
        self.console.print()
        
        # Log detalhado no arquivo
        self._save_interaction_to_file({
            "timestamp": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
            "type": "COMMAND",
            "command": command,
            "output": output,
            "return_code": return_code,
            "context": {
                "requires_deep_reasoning": getattr(self, '_current_deep_reasoning', False),
                "command_context": getattr(self, '_current_command_context', None)
            }
        })
