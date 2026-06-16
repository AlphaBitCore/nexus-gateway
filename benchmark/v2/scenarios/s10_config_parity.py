"""S-10: Config Parity Validation — Pre-flight check."""
from __future__ import annotations
from engine.config_validator import validate_config_parity
from engine.models import GatewayFullConfig

SCENARIO_ID = "S-10"

def run(configs: list[GatewayFullConfig], required_caching: bool = False, halt_on_failure: bool = True):
    return validate_config_parity(configs, required_caching=required_caching, halt_on_failure=halt_on_failure)
