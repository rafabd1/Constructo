import subprocess
import signal
import os
import shlex 
import re
import sys
from typing import Optional
import time

class LinuxInteraction:
    def __init__(self):
        self.current_process: Optional[subprocess.Popen] = None
        
    def run_command(self, command: str) -> tuple:
        """
        Execute a command and return its output
        Returns: (stdout+stderr, return_code)
        """
        try:
            # Parse command to handle redirections
            redirect_match = re.search(r'(.*?)(?:\s*>\s*(\S+))?$', command)
            if not redirect_match:
                return "Invalid command format", 1
                
            base_command = redirect_match.group(1)
            output_file = redirect_match.group(2)
            
            # Execute command and capture output
            self.current_process = subprocess.Popen(
                base_command,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,  # Redirecionar stderr para stdout
                text=True,
                shell=True,
                preexec_fn=os.setsid
            )
            
            try:
                # Capture output and wait for completion
                stdout, _ = self.current_process.communicate()
                return_code = self.current_process.returncode
                
                # Combinar stdout e stderr
                output = stdout if stdout else ""
                
                return output, return_code
                
            except KeyboardInterrupt:
                # Se processo for interrompido, matar grupo de processos
                if self.current_process:
                    try:
                        pgid = os.getpgid(self.current_process.pid)
                        os.killpg(pgid, signal.SIGINT)
                        time.sleep(0.1)
                        if self.current_process.poll() is None:
                            os.killpg(pgid, signal.SIGKILL)
                    except:
                        pass
                return "Command interrupted by user", 130
            
        except Exception as e:
            return f"Error executing command: {str(e)}", 1

    def interrupt_current_process(self):
        """Interrupt the current process if it exists"""
        if self.current_process:
            try:
                os.killpg(os.getpgid(self.current_process.pid), signal.SIGTERM)
            except:
                pass