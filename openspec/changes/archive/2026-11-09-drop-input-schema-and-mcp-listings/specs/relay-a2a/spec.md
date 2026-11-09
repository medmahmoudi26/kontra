## REMOVED Requirements

### Requirement: Input Schema Card Entry Pass-Through
**Reason**: The input-schema extension is withdrawn (D350, kontra#22). A seller card that still carries the entry is
relayed as any extension the gate does not own (`Unowned Extensions Pass Through`).
**Migration**: Sellers replace the entry with `skills[].examples` on their card.

### Requirement: Input Schema Not Validated By The Gate
**Reason**: There is no input schema to validate against; the gate already relays parts it does not interpret
(D350).
**Migration**: None. Agents validate their own input.

### Requirement: Input Schema Client Validation
**Reason**: No client renders a form from an input schema any more; `kontra connect` never did (D350).
**Migration**: An agent that needs structured input asks for it with an A2UI surface in an `input-required` turn.
