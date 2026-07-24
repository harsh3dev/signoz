import logging
from google import genai
from openinference.instrumentation.google_genai import GoogleGenAIInstrumentor
from opentelemetry import trace

# Step 3: Configure logging level
logging.getLogger().setLevel(logging.INFO)

# Instrument the Google GenAI client to capture traces
GoogleGenAIInstrumentor().instrument()

# Get an OpenTelemetry tracer
tracer = trace.get_tracer(__name__)

# Step 4: Run an example
client = genai.Client()

# Iterate through all models to find one that supports text generation
selected_model = None
print("Available models:")
for model_info in client.models.list():
    # We look for a gemini model that is newer than 2.5
    if "gemini" in model_info.name.lower() and selected_model is None:
        # Skip older models that might be deprecated for new users
        if "2.5" in model_info.name or "2.0" in model_info.name or "pro-latest" in model_info.name or "flash-latest" in model_info.name:
            continue
            
        if hasattr(model_info, 'supported_generation_methods'):
            if 'generateContent' in model_info.supported_generation_methods:
                selected_model = model_info.name
        else:
            selected_model = model_info.name

if not selected_model:
    print("\nCould not find a supported Gemini model.")
else:
    print(f"\nSelected model: {selected_model}\n")
    
    # Wrap the call in a span to attach custom business logic attributes
    # We use the same span name that the instrumentor uses ("GenerateContent")
    # so we can attach our attributes directly to the span that has the token counts.
    with tracer.start_as_current_span("GenerateContent") as span:
        # 1. Attach the Customer ID
        span.set_attribute("customer.id", "customer_globex_inc") 
        
        # 2. Attach a Prompt Name
        span.set_attribute("prompt.name", "explain_opentelemetry")
        
        response = client.models.generate_content(
            model=selected_model,
            contents="Explain OpenTelemetry in one short sentence.",
        )
        print(response.text)
        
        # Manually extract the token counts and attach them to our span
        if hasattr(response, 'usage_metadata') and response.usage_metadata:
            actual_tokens = response.usage_metadata.total_token_count
            print(f"Actual tokens used: {actual_tokens}")
            
            # FOR TESTING THE ALERT: We are faking the token count to be 2500
            # so it immediately breaches the 2000 token threshold!
            fake_token_count = 2500 
            print(f"Reporting FAKE token count to SigNoz: {fake_token_count}")
            
            span.set_attribute("llm.token_count.total", fake_token_count)
            span.set_attribute("llm.model_name", selected_model)
