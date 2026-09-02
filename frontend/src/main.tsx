import React from 'react';
import ReactDOM from 'react-dom/client';
import { ReactFlowProvider } from '@xyflow/react';
import App from './App';
import { startAddressing } from './state/addressing';
import '@xyflow/react/dist/style.css';
import './styles.css';

const root = document.getElementById('root');
if (!root) throw new Error('#root not found');

// BEFORE THE FIRST RENDER, not in an effect. The URL is what the store starts from (`address.ts`),
// so a session that opened `/runs/nscheck-123` must never see the `workflows` default — seeding
// after mount would paint the wrong surface for a frame and start its pollers. Outside React on
// purpose too: this subscribes to `popstate` for the life of the document, and StrictMode
// double-invokes effects.
startAddressing();

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <ReactFlowProvider>
      <App />
    </ReactFlowProvider>
  </React.StrictMode>
);
