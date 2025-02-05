from typing import List, Dict

PERSPECTIVE_ANALYSIS_PROMPT = """Analyze this security situation from a {perspective} mindset:

Situation: {situation}
Context: {context}

Think about:
1. What are the critical security risks?
2. What vulnerabilities could be exploited?
3. What attack vectors should we consider?
4. What defensive measures are needed?
5. What immediate actions should be taken to 

Focus on providing actionable insights from your perspective.
Be direct and specific in your analysis.
Limit response to 14 lines.

Important: Analyze in {language}."""

SYNTHESIS_PROMPT = """Let me think through everything we've learned about this situation...

Situation: {situation}

Recent conversation context:
{context}

Previous analyses:
{perspectives}

Think through each point carefully and provide a clear, direct analysis. Focus on:
1. Most critical findings
2. Greatest risks identified
3. Most urgent defensive measures
4. Specific actions needed
5. Best approach to address this

Important: Provide your analysis in plain text, not JSON format. Think in {language}."""

def _format_perspectives(self, perspectives_results: List[Dict]) -> str:
    """
    Formats the perspectives for inclusion in the synthesis prompt
    """
    formatted = []
    for p in perspectives_results:
        formatted.append(f"From {p['perspective']} viewpoint:\n{p['analysis']}\n")
    return "\n".join(formatted)

def get_perspective_prompt(language: str) -> str:
    """Returns the perspective prompt with language instruction"""
    return PERSPECTIVE_ANALYSIS_PROMPT

def get_synthesis_prompt(language: str) -> str:
    """Returns the synthesis prompt with language instruction"""
    return SYNTHESIS_PROMPT
