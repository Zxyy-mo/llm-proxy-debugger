export type AuditMode = 'compare' | 'context' | 'curl' | 'replay'
export interface InvestigationAction {
  trace: string
  action: 'context' | 'replay' | 'compare' | 'source'
}
