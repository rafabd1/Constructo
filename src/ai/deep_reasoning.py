from typing import List, Dict, Any
import asyncio
from datetime import datetime
from ai.rate_limiter import RateLimiter
from prompts.deep_reasoning_prompts import PERSPECTIVE_ANALYSIS_PROMPT, SYNTHESIS_PROMPT, get_synthesis_prompt
import json
import re
import traceback
import google.generativeai as genai


class DeepReasoning:
    def __init__(self, agent):
        """Initialize Deep Reasoning with agent configuration"""
        self.agent = agent
        self.config = agent.config.get('deep_reasoning', {})
        self.perspectives = self.config.get('perspectives', {})
        self.activation_triggers = self.config.get('activation_triggers', {})
        self.consecutive_failures = 0
        self.command_history = []
        self.language = agent.config.get('agent', {}).get('language', 'en-US')
        
        # Create separate model for Deep Reasoning but use the same rate limiter
        self.model = genai.GenerativeModel(
            self.agent.config.get('model', {}).get('name', 'gemini-2.0-flash-exp')
        )
        self.rate_limiter = self.agent.rate_limiter
        
        if not PERSPECTIVE_ANALYSIS_PROMPT or not SYNTHESIS_PROMPT:
            raise ValueError("Deep Reasoning prompts not properly imported")
            
    def should_activate(self, response: Dict) -> bool:
        """
        Determine if deep reasoning should be activated based on:
        1. Explicit request from response
        2. Consecutive failures threshold
        """
        # Ativar se explicitamente solicitado
        if response.get("requires_deep_reasoning", False):
            return True
        
        # Ativar se atingiu limite de falhas consecutivas
        if self.consecutive_failures >= self.activation_triggers.get('consecutive_failures', 4):
            self.agent.terminal.log(
                f"Activating Deep Reasoning - {self.consecutive_failures} consecutive failures detected",
                "INFO"
            )
            return True
        
        return False
        
    def record_result(self, success: bool):
        """Records success/failure to track consecutive failures"""
        if success:
            self.consecutive_failures = 0
        else:
            self.consecutive_failures += 1
            
    async def _send_message(self, prompt: str, config: Dict = None) -> str:
        """Send message using dedicated Deep Reasoning model"""
        max_attempts = self.agent.retry_config.get('max_attempts', 3)
        retry_delay = self.agent.retry_config.get('delay_between_retries', 20)
        last_error = None
        
        for attempt in range(max_attempts):
            try:
                await self.rate_limiter.wait_if_needed_async()
                
                if config:
                    generation_config = genai.GenerationConfig(**config)
                else:
                    generation_config = self.agent.generation_config
                
                response = self.model.generate_content(
                    prompt,
                    generation_config=generation_config
                )
                
                if response and response.text:
                    return response.text
                raise ValueError("Empty response from API")
                
            except Exception as e:
                last_error = e
                error_msg = str(e)
                
                if "429" in error_msg or "quota" in error_msg.lower():
                    if attempt < max_attempts - 1:
                        self.agent.terminal.log(
                            f"Rate limit reached in Deep Reasoning. Waiting {retry_delay}s before retry {attempt + 1}/{max_attempts}",
                            "WARNING"
                        )
                        await asyncio.sleep(retry_delay)
                        continue
                elif attempt < max_attempts - 1:
                    self.agent.terminal.log(
                        f"Deep Reasoning API error: {error_msg}. Retrying {attempt + 1}/{max_attempts}...",
                        "WARNING"
                    )
                    await asyncio.sleep(1)
                    continue
                
        raise last_error or Exception("Maximum retry attempts reached")

    async def deep_analyze(self, situation: str, context: str = "") -> dict:
        try:
            self.agent.terminal.start_deep_reasoning()
            perspectives_results = []
            
            # Obter histórico do chat
            chat_history = self.agent.chat.history[-15:]  # Últimas 15 mensagens
            formatted_history = "\n".join([
                f"[{msg.role}]: {msg.parts[0].text if msg.parts else ''}"
                for msg in chat_history
                if hasattr(msg, 'role') and hasattr(msg, 'parts')
            ])
            
            # Obter a linguagem configurada
            language = self.agent.config.get('agent', {}).get('language', 'en-US')
            
            # Analisar perspectivas
            for perspective_name, perspective_cfg in self.perspectives.items():
                self.agent.terminal.log_deep_reasoning_step(
                    f"Analyzing with {perspective_name} perspective..."
                )
                
                try:
                    formatted_prompt = PERSPECTIVE_ANALYSIS_PROMPT.format(
                        perspective=perspective_name,
                        situation=str(situation),
                        context=formatted_history,
                        language=language
                    )
                    
                    response = await self._send_message(formatted_prompt, perspective_cfg)
                    
                    if not response:
                        raise ValueError(f"Empty response received for {perspective_name}")
                    
                    perspective_result = {
                        "perspective": perspective_name,
                        "analysis": response
                    }
                    
                    perspectives_results.append(perspective_result)
                    
                except Exception as e:
                    self.agent.terminal.log(
                        f"Error in {perspective_name} analysis: {str(e)}", 
                        "ERROR"
                    )
                    continue
            
            # Adicionar o thinking como último step
            self.agent.terminal.log_deep_reasoning_step("Thinking through all perspectives...")
            
            try:
                # Sintetizar perspectivas
                synthesis_result = await self._synthesize_perspectives(
                    perspectives_results, 
                    situation,
                    formatted_history,
                    language
                )
                
                # Parar o spinner do Deep Reasoning
                self.agent.terminal.stop_processing()
                
                # Adicionar a síntese ao histórico do chat
                self.agent.chat.history.append({
                    "role": "assistant",
                    "parts": [synthesis_result["message"]]
                })
                
                # Retornar apenas a análise para o agente decidir o próximo passo
                return {
                    "type": "analysis",
                    "message": synthesis_result["message"],
                    "requires_deep_reasoning": False,
                    "continue": True
                }
                
            except Exception as e:
                error_msg = f"Deep Reasoning failed: {str(e)}"
                self.agent.terminal.log(error_msg, "ERROR")
                
                return {
                    "type": "response",
                    "message": "Deep analysis failed. Please proceed with standard analysis.",
                    "requires_deep_reasoning": False,
                    "continue": True
                }
                
        except Exception as e:
            self.agent.terminal.log(f"Deep Reasoning failed: {str(e)}", "ERROR")
            return {
                "type": "response",
                "message": str(e),
                "requires_deep_reasoning": False,
                "continue": True
            }
    
    def _temp_configure_model(self, config: Dict) -> Dict:
        """
        Temporarily configures the model with new parameters
        Uses agent's model configuration methods
        """
        # Use agent's configuration method
        return self.agent._temp_configure_model({
            "temperature": config.get("temperature", 0.5),
            "top_p": config.get("top_p", 0.7),
            "top_k": config.get("top_k", 40)
        })
    
    def _restore_model_config(self, original_config: Dict):
        """
        Restores the original model configuration
        Uses agent's model configuration methods
        """
        # Use agent's restore method
        self.agent._restore_model_config(original_config)
    
    def _extract_json(self, text: str) -> str:
        """Extract JSON from text, handling various formats"""
        # Remove leading/trailing whitespace
        text = text.strip()
        
        # Find JSON boundaries
        start = text.find('{')
        end = text.rfind('}') + 1
        
        if start >= 0 and end > 0:
            return text[start:end]
        
        raise ValueError("No valid JSON found in response")
    
    async def _synthesize_perspectives(self, perspectives_results: List[Dict], situation: str, context: str, language: str) -> Dict:
        """
        Synthesizes different perspectives into a final analysis with streaming
        """
        synthesis_prompt = self._create_synthesis_prompt(
            perspectives_results, 
            situation,
            context,
            language
        )
        
        try:
            # Stop the spinner before starting synthesis
            self.agent.terminal.stop_processing()
            
            # Use shared rate limiter
            await self.rate_limiter.wait_if_needed_async()
            
            response = self.model.generate_content(
                synthesis_prompt,
                stream=True,
                generation_config=self.agent.generation_config
            )
            
            full_response = ""
            buffer = ""
            last_char = " "
            
            for chunk in response:
                if hasattr(chunk, 'parts') and chunk.parts:
                    for part in chunk.parts:
                        if hasattr(part, 'text'):
                            text = str(part.text)
                            
                            # Ignorar qualquer coisa que pareça JSON
                            if text.strip().startswith('{') or text.strip().startswith('```'):
                                continue
                            
                            text = text.replace('\r', '')
                            
                            # Remove duplicate content
                            if text in full_response:
                                continue
                            
                            full_response += text
                            
                            # Process text character by character
                            for char in text:
                                buffer += char
                                
                                # Print character by character with different delays
                                if char in ['.', '!', '?']:
                                    self.agent.terminal.console.print(buffer, style="dim", end="")
                                    buffer = ""
                                    await asyncio.sleep(0.1)
                                elif char in [',', ';', ':']:
                                    self.agent.terminal.console.print(buffer, style="dim", end="")
                                    buffer = ""
                                    await asyncio.sleep(0.05)
                                elif char == '\n':
                                    if buffer.strip():
                                        self.agent.terminal.console.print(buffer, style="dim")
                                        buffer = ""
                                    await asyncio.sleep(0.02)
                                elif len(buffer) > 2:
                                    self.agent.terminal.console.print(buffer, style="dim", end="")
                                    buffer = ""
                                    await asyncio.sleep(0.01)
                                
                                last_char = char
            
            # Print any remaining text
            if buffer:
                self.agent.terminal.console.print(buffer, style="dim")
            
            # Add final line break
            self.agent.terminal.console.print()
            
            # Limpar o texto final de qualquer JSON ou marcações
            clean_response = re.sub(r'```.*?```', '', full_response, flags=re.DOTALL)
            clean_response = re.sub(r'\{.*?\}', '', clean_response, flags=re.DOTALL)
            clean_response = clean_response.strip()
            
            # Parar o spinner do Deep Reasoning
            self.agent.terminal.stop_processing()
            
            # Retornar a análise para o agente continuar
            return {
                "type": "analysis",
                "message": clean_response,
                "requires_deep_reasoning": False,
                "continue": True
            }
            
        except Exception as e:
            error_msg = f"\nError in synthesis: {str(e)}"
            self.agent.terminal.log(error_msg, "ERROR")
            
            return {
                "type": "response",
                "message": "Deep analysis failed. Please proceed with standard analysis.",
                "requires_deep_reasoning": False,
                "continue": True
            }
    
    def _create_synthesis_prompt(self, perspectives_results: List[Dict], situation: str, context: str, language: str) -> str:
        """Creates the prompt to synthesize different perspectives"""
        return get_synthesis_prompt(language).format(
            situation=situation,
            perspectives=self._format_perspectives(perspectives_results),
            context=context,  # Incluir contexto do chat
            language=language
        )

    def _format_perspectives(self, perspectives_results: List[Dict]) -> str:
        """
        Formats perspectives for inclusion in synthesis prompt
        """
        formatted = []
        for p in perspectives_results:
            formatted.append(f"Perspective {p['perspective']}:\n{p['analysis']}\n")
        return "\n".join(formatted)

    def _validate_json(self, json_str: str) -> Dict:
        """
        Validate and clean JSON string with multiple fallbacks
        """
        try:
            # First try direct parse
            return json.loads(json_str)
        except json.JSONDecodeError as e:
            # Try removing markdown
            clean_text = re.sub(r"```json\s*([\s\S]*?)```", r"\1", json_str)
            clean_text = clean_text.strip()
            
            # Try fixing common issues
            clean_text = clean_text.replace("'", '"')  # Replace single quotes
            clean_text = re.sub(r"(\w+):", r'"\1":', clean_text)  # Add quotes to keys
            clean_text = re.sub(r":\s*(\w+)([,\}])", r':"\1"\2', clean_text)  # Add quotes to unquoted values
            
            try:
                return json.loads(clean_text)
            except json.JSONDecodeError:
                # If still failing, try to extract valid JSON portion
                start = clean_text.find('{')
                end = clean_text.rfind('}') + 1
                if start >= 0 and end > 0:
                    try:
                        return json.loads(clean_text[start:end])
                    except json.JSONDecodeError:
                        pass
                    
                # If all else fails, return error structure
                return {
                    "error": "Invalid JSON format",
                    "original": json_str[:100] + "..." if len(json_str) > 100 else json_str
                }