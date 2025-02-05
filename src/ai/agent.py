import asyncio
import google.generativeai as genai
from google.api_core import retry
import time
import json
import re 
from datetime import datetime
from core.terminal import UnifiedTerminal
from core.linux_interaction import LinuxInteraction
from prompts.main_context_prompt import get_system_prompt
from ai.context_manager import ContextManager
from ai.rate_limiter import RateLimiter
from ai.deep_reasoning import DeepReasoning
from typing import Dict, Optional, Callable

def _extract_json(text: str) -> str:
    """
    Extract and clean JSON from text with multiple fallback strategies
    """
    # Remove leading/trailing whitespace
    text = text.strip()
    
    # Strategy 1: Find JSON between curly braces
    def find_json_boundaries(text: str) -> tuple:
        stack = []
        start = -1
        
        for i, char in enumerate(text):
            if char == '{':
                if not stack:
                    start = i
                stack.append(char)
            elif char == '}':
                if stack:
                    stack.pop()
                    if not stack and start != -1:
                        return start, i + 1
        return -1, -1
    
    # Strategy 2: Clean and fix common JSON issues
    def clean_json_text(text: str) -> str:
        # Remove markdown code blocks
        text = re.sub(r"```(?:json)?\s*([\s\S]*?)```", r"\1", text)
        
        # Fix common JSON syntax issues
        text = text.replace("'", '"')  # Replace single quotes
        text = re.sub(r'([{,])\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*:', r'\1"\2":', text)  # Add quotes to keys
        text = re.sub(r':\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*([,}])', r':"\1"\2', text)  # Add quotes to string values
        text = re.sub(r'//(.*?)\n', '\n', text)  # Remove inline comments
        text = re.sub(r'/\*.*?\*/', '', text, flags=re.DOTALL)  # Remove block comments
        
        return text
    
    # Try Strategy 1: Find valid JSON
    start, end = find_json_boundaries(text)
    if start >= 0 and end > 0:
        try:
            json_str = text[start:end]
            json.loads(json_str)  # Validate JSON
            return json_str
        except json.JSONDecodeError:
            pass
    
    # Try Strategy 2: Clean and fix JSON
    cleaned_text = clean_json_text(text)
    start, end = find_json_boundaries(cleaned_text)
    if start >= 0 and end > 0:
        try:
            json_str = cleaned_text[start:end]
            json.loads(json_str)  # Validate JSON
            return json_str
        except json.JSONDecodeError:
            pass
    
    # Try Strategy 3: Extract any JSON-like structure
    json_pattern = r'{[\s\S]*}'
    matches = re.findall(json_pattern, cleaned_text)
    for match in matches:
        try:
            json.loads(match)  # Validate JSON
            return match
        except json.JSONDecodeError:
            continue
    
    raise ValueError("No valid JSON found in response")

class AIAgent:
    def __init__(self, config: dict):
        self.config = config
        genai.configure(api_key=self.config['api_key'])
        model_config = self.config.get('model', {})
        self.generation_config = genai.GenerationConfig(
            temperature=model_config.get('temperature', 0.7),
            top_p=model_config.get('top_p', 0.9),
            top_k=model_config.get('top_k', 40),
            max_output_tokens=model_config.get('max_output_tokens', 4096),
        )
        self.model = genai.GenerativeModel(
            model_config.get('name', 'gemini-2.0-flash-exp'),
            generation_config=self.generation_config 
        )
        
        # Obter e armazenar o prompt do sistema
        self.system_prompt = get_system_prompt(config)
        
        self.chat = self.model.start_chat(history=[
            {"role": "user", "parts": [self.system_prompt]},
            {"role": "model", "parts": ["System initialized with instructions. Ready to execute commands."]}
        ])
        self.terminal = UnifiedTerminal()
        self.linux = LinuxInteraction()
        self.context_manager = ContextManager()
        
        # Initialize rate limiter
        api_config = config.get('api', {}).get('rate_limit', {})
        self.rate_limiter = RateLimiter(
            requests_per_minute=api_config.get('requests_per_minute', 30),
            delay_between_requests=api_config.get('delay_between_requests', 0.5)
        )
        
        # Retry configuration
        self.retry_config = config.get('api', {}).get('retry', {})
        
        # Risk level weights for comparison
        self.risk_levels = {
            "none": 0,
            "low": 1,
            "medium": 2,
            "high": 3
        }
        
        self.deep_reasoning = DeepReasoning(self)
        
        self.current_task: Optional[asyncio.Task] = None
        self.terminal.set_interrupt_handler(self.handle_interrupt)
        
    def _initialize_chat(self):
        genai.configure(api_key=self.config['api_key'])
        
        model_config = self.config.get('model', {})
        
        model = genai.GenerativeModel(
            model_config.get('name', 'gemini-2.0-flash-exp'),
            generation_config=self.generation_config
        )
        
        # Usar o prompt do sistema armazenado
        return model.start_chat(history=[
            {"role": "user", "parts": [self.system_prompt]},
            {"role": "model", "parts": ["System initialized with instructions. Ready to execute commands."]}
        ])
        
    def _needs_confirmation(self, risk_level: str) -> bool:
        """Determine if an action needs user confirmation based on config"""
        agent_config = self.config.get('agent', {})
        
        if not agent_config.get('require_confirmation', True):
            return False
            
        threshold = agent_config.get('risk_threshold', 'medium').lower()
        
        action_risk = self.risk_levels.get(risk_level.lower(), 0)
        threshold_risk = self.risk_levels.get(threshold, 2)  # default to medium
        
        return action_risk > threshold_risk
        
    async def _send_message_with_retry(self, message: str) -> str:
        """Send message to API with retry logic"""
        max_attempts = self.retry_config.get('max_attempts', 3)
        retry_delay = self.retry_config.get('delay_between_retries', 10)
        last_error = None
        
        for attempt in range(max_attempts):
            try:
                # Esperar antes de fazer a requisição
                await self.rate_limiter.wait_if_needed_async()  # Mudado para versão async
                
                response = self.chat.send_message(message)
                if response and response.text:
                    return response.text
                raise ValueError("Empty response from API")
                
            except Exception as e:
                last_error = e
                error_msg = str(e)
                
                if "429" in error_msg or "quota" in error_msg.lower():
                    if attempt < max_attempts - 1:
                        self.terminal.log(
                            f"Rate limit reached. Waiting {retry_delay}s before retry {attempt + 1}/{max_attempts}",
                            "WARNING"
                        )
                        await asyncio.sleep(retry_delay)
                        continue
                elif attempt < max_attempts - 1:
                    self.terminal.log(
                        f"API error: {error_msg}. Retrying {attempt + 1}/{max_attempts}...",
                        "WARNING"
                    )
                    await asyncio.sleep(1)
                    continue
                
        raise last_error or Exception("Maximum retry attempts reached")
        
    def handle_interrupt(self):
        """Handler for command interruption"""
        if self.current_task and not self.current_task.done():
            self.current_task.cancel()
        if hasattr(self.linux, 'current_process'):
            self.linux.interrupt_current_process()
            
    async def execute_step(self, parsed_response, context_info: str = None):
        try:
            # Check if deep reasoning is needed
            should_activate = parsed_response.get("requires_deep_reasoning", False)
            
            # Se já estiver em deep reasoning, não ativar novamente
            if hasattr(self, '_in_deep_reasoning'):
                should_activate = False
            
            if should_activate:
                # Marcar que está em deep reasoning
                self._in_deep_reasoning = True
                
                try:
                    # Passar o contexto_info diretamente para deep_analyze
                    deep_analysis = await self.deep_reasoning.deep_analyze(
                        context_info or "Current situation analysis",
                        ""  # Não precisa passar contexto, será obtido do chat.history
                    )
                    
                    # Limpar flag após análise
                    if hasattr(self, '_in_deep_reasoning'):
                        delattr(self, '_in_deep_reasoning')
                    
                    # Processar a resposta do deep_analysis
                    if isinstance(deep_analysis, dict):
                        # Adicionar a síntese ao histórico
                        self.chat.history.append({
                            "role": "assistant",
                            "parts": [deep_analysis["message"]]
                        })
                        
                        # Enviar análise para o modelo principal
                        analysis_prompt = f"""System: {self.system_prompt}
                        
                        A deep analysis has been performed. Based on this analysis, determine the best action to take:

                        Deep Reasoning Analysis:
                        {deep_analysis["message"]}

                        IMPORTANT: Do not include any commands in your response. Just analyze the situation and provide guidance.
                        Please respond in the specified JSON format with type: "analysis" and continue: true."""
                        
                        # Obter resposta do modelo
                        response = self.model.generate_content(analysis_prompt)
                        
                        if hasattr(response, 'parts') and response.parts:
                            try:
                                # Processar resposta
                                response_text = response.parts[0].text
                                
                                # Não mostrar o JSON no terminal
                                clean_text = re.sub(r'```json.*?```', '', response_text, flags=re.DOTALL)
                                clean_text = re.sub(r'\{.*?\}', '', clean_text, flags=re.DOTALL)
                                
                                if clean_text.strip():
                                    self.terminal.log_agent(clean_text.strip())
                                
                                # Retornar para continuar o fluxo normal
                                return deep_analysis["message"], True
                                
                            except Exception as e:
                                self.terminal.log(f"Error processing model response: {str(e)}", "ERROR")
                        
                        # Fallback: continuar com a análise original
                        return deep_analysis["message"], True
                    
                    return str(deep_analysis), False
                    
                except Exception as e:
                    # Limpar flag em caso de erro
                    if hasattr(self, '_in_deep_reasoning'):
                        delattr(self, '_in_deep_reasoning')
                        
                    error_msg = f"Deep Reasoning failed: {str(e)}"
                    self.terminal.log(error_msg, "ERROR")
                    return error_msg, False
            
            # Execute command if present
            if parsed_response.get("next_step"):
                action = parsed_response["next_step"]
                if action.get("command"):
                    # Verificar confirmação antes de qualquer log
                    if self._needs_confirmation(action.get("risk", "low")):
                        if not await self.terminal.request_confirmation(
                            f"Execute command '{action['command']}'? This action has {action.get('risk', 'unknown')} risk."
                        ):
                            return "Command cancelled by user", False
                    
                    # Executar comando
                    output, returncode = self.linux.run_command(action['command'])
                    
                    # Log do comando e output
                    self.terminal.log_command(action['command'], output, returncode)
                    
                    # Se o comando falhou, retornar erro
                    if returncode != 0:
                        return f"Command failed with code {returncode}: {output}", False
                    
                    # Processar output do comando
                    return output, parsed_response.get("continue", False)
            
            # Se não tiver next_step, retornar apenas a mensagem
            return parsed_response.get("message", ""), parsed_response.get("continue", False)
            
        except Exception as e:
            self.terminal.log(f"Error in execute_step: {str(e)}", "ERROR")
            return str(e), False
        finally:
            # Limpar contadores e flags
            if hasattr(self, '_execution_depth'):
                self._execution_depth -= 1
                if self._execution_depth == 0:
                    delattr(self, '_execution_depth')
            
            # Clear context after execution
            self.terminal._current_deep_reasoning = False
            self.terminal._current_command_context = None
            self.terminal._current_analysis_type = None
            self.terminal._last_raw_response = None

    async def process_command(self, user_input: str):
        try:
            # Store current task
            self.current_task = asyncio.current_task()
            
            # Temp
            if hasattr(self, '_current_command') and self._current_command == user_input:
                return "Command already being processed. Please wait."
            
            self._current_command = user_input
            
            context = self.context_manager.get_current_context()
            current_time = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
            
            self.terminal.start_processing("Thinking...")
            
            prompt = f"""System: {self.system_prompt}
            Current context: {context}
            User command: {user_input}
            Timestamp: {current_time}
            
            Analyze the input and respond in the specified JSON format."""
            
            response_text = await self._send_message_with_retry(prompt)
            
            # Save raw response for logging
            self.terminal._last_raw_response = response_text
            
            if not response_text:
                raise ValueError("Received empty response from the chat model.")
            
            self.terminal.stop_processing()
            
            async def process_response(response_text: str):
                try:
                    # Se a resposta já for um objeto JSON
                    if isinstance(response_text, dict):
                        parsed = response_text
                    else:
                        # Inicializar variáveis fora do bloco try
                        clean_text = ""
                        json_str = ""
                        
                        try:
                            clean_text = re.sub(r"```json\s*([\s\S]*?)```", r"\1", response_text)
                            json_str = _extract_json(clean_text)
                            parsed = json.loads(json_str)
                        except Exception as e:
                            # Se falhar ao extrair JSON, tentar usar a resposta como mensagem simples
                            return {
                                "type": "response",
                                "message": str(response_text),
                                "requires_deep_reasoning": False,
                                "continue": False
                            }
                    
                    # Log do parsing inicial
                    self.terminal._save_interaction_to_file({
                        "timestamp": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                        "type": "RESPONSE_PARSE_DEBUG",
                        "cleaned_text": clean_text,
                        "extracted_json": json_str,
                        "parsed_result": parsed
                    })
                    
                    # Verificar se já processamos esta resposta
                    if isinstance(response_text, str):
                        response_hash = hash(response_text)
                    else:
                        response_hash = hash(str(response_text))
                        
                    if hasattr(self, '_last_response_hash') and self._last_response_hash == response_hash:
                        return "Response already processed"
                    self._last_response_hash = response_hash
                    
                    # Exibir mensagem do agente se existir
                    if parsed.get("message"):
                        self.terminal.log_agent(parsed["message"])
                    
                    # Passar user_input como contexto
                    result, should_continue = await self.execute_step(parsed, user_input)
                    
                    # Limpar hash se não precisar continuar
                    if not should_continue:
                        delattr(self, '_last_response_hash')
                        return result
                    
                    # Se precisar continuar e tiver resultado
                    if should_continue and parsed.get("continue", False):
                        # Se o resultado já for um objeto JSON
                        if isinstance(result, dict):
                            return await process_response(result)
                        # Se for string, tentar processar como JSON
                        try:
                            return await process_response(result)
                        except:
                            # Se falhar, apenas retornar erro em formato JSON
                            return {
                                "type": "response",
                                "message": f"Error processing response: {str(result)}",
                                "requires_deep_reasoning": False,
                                "continue": False
                            }
                    
                    return result
                    
                except Exception as e:
                    self.terminal.log(f"Error in process_response: {str(e)}", "ERROR")
                    return str(e)

            final_result = await process_response(response_text)
            return final_result.strip() if isinstance(final_result, str) else str(final_result)

        except asyncio.CancelledError:
            self.terminal.log("Command execution cancelled", "WARNING")
            return "Command cancelled by user"
        except Exception as e:
            error_msg = f"Error: {str(e)}"
            self.terminal.log(error_msg, "ERROR")
            return error_msg
        finally:
            self.current_task = None

    def _temp_configure_model(self, config: Dict) -> Dict:
        """
        Temporarily configures the model with new parameters and returns original configuration.
        """
        original_config = {
            "temperature": self.generation_config.temperature,
            "top_p": self.generation_config.top_p,
            "top_k": self.generation_config.top_k
        }
        self.generation_config.temperature = config.get("temperature", original_config["temperature"])
        self.generation_config.top_p = config.get("top_p", original_config["top_p"])
        self.generation_config.top_k = config.get("top_k", original_config["top_k"])
        return original_config

    def _restore_model_config(self, original_config: Dict):
        """
        Restores the model configuration to the previously saved settings.
        """
        self.generation_config.temperature = original_config["temperature"]
        self.generation_config.top_p = original_config["top_p"]
        self.generation_config.top_k = original_config["top_k"]

    async def _process_deep_reasoning_response(self, response: str) -> dict:
        """Process and validate deep reasoning response"""
        try:
            # Extract and parse JSON
            clean_text = re.sub(r"```json\s*([\s\S]*?)```", r"\1", response)
            json_str = _extract_json(clean_text)
            parsed = json.loads(json_str)
            
            # Log do parsing para debug
            self.terminal._save_interaction_to_file({
                "timestamp": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                "type": "DEEP_REASONING_PARSE_DEBUG",
                "cleaned_text": clean_text,
                "extracted_json": json_str,
                "parsed_result": parsed
            })
            
            # Validate response structure
            if not isinstance(parsed, dict):
                raise ValueError("Response must be a JSON object")
            
            # Ensure type field exists and is valid
            if "type" not in parsed or parsed["type"] not in ["response", "command", "analysis"]:
                parsed["type"] = "response"
            
            # Ensure message field exists
            if "message" not in parsed:
                parsed["message"] = ""
            
            # Validate next_step and requires_deep_reasoning
            # Não permitir ambos ao mesmo tempo
            if parsed.get("next_step") and parsed.get("requires_deep_reasoning"):
                # Priorizar o next_step e desativar deep_reasoning
                parsed["requires_deep_reasoning"] = False
            
            # Validate next_step if present
            if "next_step" in parsed and parsed["next_step"]:
                if not isinstance(parsed["next_step"], dict):
                    parsed["next_step"] = None
                else:
                    if "command" not in parsed["next_step"]:
                        parsed["next_step"] = None
            
            # Ensure required boolean fields
            parsed["continue"] = parsed.get("continue", False)
            
            return parsed
            
        except Exception as e:
            self.terminal._save_interaction_to_file({
                "timestamp": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                "type": "DEEP_REASONING_PARSE_ERROR",
                "error": str(e),
                "raw_response": response
            })
            
            # Em caso de erro, tentar retornar um objeto válido com a mensagem
            try:
                # Se conseguirmos extrair algum texto útil, usamos como mensagem
                clean_text = re.sub(r"```.*?```", "", response, flags=re.DOTALL)
                clean_text = re.sub(r"\n+", " ", clean_text).strip()
                if clean_text:
                    return {
                        "type": "response",
                        "message": clean_text,
                        "requires_deep_reasoning": False,
                        "continue": False
                    }
            except:
                pass
            
            return {
                "type": "response",
                "message": "Error processing deep reasoning analysis",
                "requires_deep_reasoning": False,
                "continue": False
            }
