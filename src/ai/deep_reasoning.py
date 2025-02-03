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
                f"[{msg['role']}]: {msg['parts'][0]}"
                for msg in chat_history
                if msg['parts'] and msg['parts'][0]
            ])
            
            # Obter a linguagem configurada
            language = self.agent.config.get('agent', {}).get('language', 'en-US')
            
            for perspective_name, perspective_cfg in self.perspectives.items():
                self.agent.terminal.log_deep_reasoning_step(
                    f"Analyzing with {perspective_name} perspective..."
                )
                
                try:
                    formatted_prompt = PERSPECTIVE_ANALYSIS_PROMPT.format(
                        perspective=perspective_name,
                        situation=str(situation),
                        context=formatted_history,  # Usar histórico do chat
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
                    
                    # Adicionar perspectiva ao contexto
                    self.agent.context_manager.add_to_context({
                        "timestamp": datetime.now().strftime("%H:%M:%S"),
                        "type": "PERSPECTIVE",
                        "content": f"{perspective_name}: {response[:100]}..."  # Primeiros 100 caracteres
                    })
                    
                except Exception as e:
                    self.agent.terminal.log(
                        f"Error in {perspective_name} analysis: {str(e)}", 
                        "ERROR"
                    )
                    continue
            
            # Adicionar o thinking como último step
            self.agent.terminal.log_deep_reasoning_step("Thinking through all perspectives...")
            
            try:
                final_analysis = await self._synthesize_perspectives(
                    perspectives_results, 
                    situation,
                    formatted_history,  # Passar histórico para síntese
                    language
                )
                synthesis_result = final_analysis["analysis"]
                synthesis_response = final_analysis["next_step"]
            except Exception as synthesis_error:
                # Se a síntese falhar, criar uma síntese manual das perspectivas
                self.agent.terminal.log(f"Synthesis failed: {str(synthesis_error)}", "WARNING")
                
                # Combinar análises de forma estruturada
                combined_analysis = {
                    "type": "analysis",
                    "message": "Baseado na análise de múltiplas perspectivas:\n\n" + 
                              "\n\n".join([
                                  f"Da perspectiva {p['perspective']}:\n{p['analysis']}"
                                  for p in perspectives_results
                              ]),
                    "requires_deep_reasoning": False,
                    "continue": True
                }
                
                synthesis_result = combined_analysis
                synthesis_response = None
            
            # Log da síntese antes do processamento
            self.agent.terminal._save_interaction_to_file({
                "timestamp": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                "type": "DEEP_REASONING_SYNTHESIS",
                "content": synthesis_result,
                "raw_synthesis": synthesis_response
            })
            
            try:
                # Garantir que a síntese está no formato JSON correto
                if isinstance(synthesis_result, str):
                    synthesis_result = {
                        "type": "analysis",
                        "message": synthesis_result,
                        "requires_deep_reasoning": False,
                        "continue": True
                    }
                
                # Adicionar síntese ao contexto
                self.agent.context_manager.add_to_context({
                    "timestamp": datetime.now().strftime("%H:%M:%S"),
                    "type": "SYNTHESIS",
                    "content": synthesis_result.get("message", "")[:200] + "..."  # Primeiros 200 caracteres
                })
                
                return synthesis_result
                
            except Exception as e:
                self.agent.terminal._save_interaction_to_file({
                    "timestamp": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
                    "type": "DEEP_REASONING_ERROR",
                    "error": str(e),
                    "synthesis_result": synthesis_result
                })
                raise e
            
        except Exception as e:
            self.agent.terminal.log(f"Deep Reasoning failed: {str(e)}", "ERROR")
            return {"type": "error", "message": str(e)}
    
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
        """Synthesizes different perspectives into a final analysis"""
        synthesis_prompt = self._create_synthesis_prompt(
            perspectives_results, 
            situation,
            context,  # Passar contexto do chat
            language
        )
        
        try:
            # Use shared rate limiter
            await self.rate_limiter.wait_if_needed_async()
            
            response = self.model.generate_content(
                synthesis_prompt,
                stream=True,
                generation_config=self.agent.generation_config
            )
            
            full_response = ""
            buffer = ""
            
            for chunk in response:
                if hasattr(chunk, 'text') and chunk.text:
                    text = chunk.text
                    full_response += text
                    buffer += text
                    
                    # Quando tiver texto suficiente ou encontrar pontuação
                    if len(buffer) > 50 or any(p in buffer for p in ['.', '!', '?', '\n']):
                        self.agent.terminal.log(buffer, "DIM", show_timestamp=False)
                        buffer = ""
                        await asyncio.sleep(0.01)
            
            # Imprimir qualquer texto restante
            if buffer:
                self.agent.terminal.log(buffer, "DIM", show_timestamp=False)
            
            return {
                "type": "analysis",
                "analysis": full_response,
                "next_step": None
            }
            
        except Exception as e:
            error_msg = f"\nError in synthesis: {str(e)}"
            self.agent.terminal.log(error_msg, "ERROR")
            
            error_response = "Based on the deep analysis performed, please evaluate the results and determine the next action."
            
            return {
                "type": "response",
                "message": error_response,
                "next_step": None
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