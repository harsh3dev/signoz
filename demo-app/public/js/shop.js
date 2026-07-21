// Shop client-side application logic
document.addEventListener('DOMContentLoaded', () => {
  const productsContainer = document.getElementById('products-container');
  const cartEmptyMessage = document.getElementById('cart-empty-message');
  const cartItemsList = document.getElementById('cart-items-list');
  const cartTotalSection = document.getElementById('cart-total-section');
  const cartTotalAmount = document.getElementById('cart-total-amount');
  const checkoutForm = document.getElementById('checkout-form');
  const checkoutBtn = document.getElementById('checkout-btn');

  let products = [];
  let cart = JSON.parse(localStorage.getItem('telemetry-cart')) || [];

  // Initialize
  fetchProducts();
  updateCartUI();

  // Fetch product listings from API
  async function fetchProducts() {
    try {
      const res = await fetch('/api/products');
      if (!res.ok) throw new Error('Failed to retrieve inventory.');
      products = await res.json();
      renderProducts();
    } catch (err) {
      console.error('Failed to load products list:', err);
      productsContainer.innerHTML = `
        <div style="grid-column: 1/-1; text-align: center; padding: 3rem 0; color: var(--error);">
          <p>Failed to retrieve products inventory.</p>
          <button class="btn btn-outline" style="margin-top: 1rem;" onclick="location.reload()">Retry Connection</button>
        </div>
      `;
    }
  }

  // Render product grids
  function renderProducts() {
    if (products.length === 0) {
      productsContainer.innerHTML = `
        <div style="grid-column: 1/-1; text-align: center; padding: 3rem 0; color: var(--text-muted);">
          No merchandise available on the shelf.
        </div>
      `;
      return;
    }

    productsContainer.innerHTML = products.map(product => `
      <div class="product-card">
        <img src="${product.image}" alt="${product.name}" class="product-image" onerror="this.src='https://images.unsplash.com/photo-1531403009284-440f080d1e12?w=300&q=80'">
        <div class="product-info">
          <div class="product-name">${product.name}</div>
          <div class="product-desc">${product.description}</div>
          <div class="product-footer">
            <div class="product-price">$${(product.price / 100).toFixed(2)}</div>
            <button class="btn" onclick="addToCart('${product.id}')">
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <circle cx="9" cy="21" r="1"></circle>
                <circle cx="20" cy="21" r="1"></circle>
                <path d="M1 1h4l2.68 13.39a2 2 0 0 0 2 1.61h9.72a2 2 0 0 0 2-1.61L23 6H6"></path>
              </svg>
              Add to Cart
            </button>
          </div>
        </div>
      </div>
    `).join('');
  }

  // Add to cart helper exposed globally
  window.addToCart = function(productId) {
    const product = products.find(p => p.id === productId);
    if (!product) return;

    const existingItem = cart.find(item => item.id === productId);
    if (existingItem) {
      existingItem.quantity += 1;
    } else {
      cart.push({
        id: product.id,
        name: product.name,
        price: product.price,
        quantity: 1
      });
    }

    saveCart();
    updateCartUI();
    showToast(`Added "${product.name}" to cart!`, 'success');
  };

  // Remove item or decrement qty
  window.removeFromCart = function(productId) {
    const itemIndex = cart.findIndex(item => item.id === productId);
    if (itemIndex === -1) return;

    if (cart[itemIndex].quantity > 1) {
      cart[itemIndex].quantity -= 1;
    } else {
      cart.splice(itemIndex, 1);
    }

    saveCart();
    updateCartUI();
  };

  function saveCart() {
    localStorage.setItem('telemetry-cart', JSON.stringify(cart));
  }

  // Update Shopping Cart Side Panel UI
  function updateCartUI() {
    if (cart.length === 0) {
      cartEmptyMessage.style.display = 'block';
      cartItemsList.style.display = 'none';
      cartTotalSection.style.display = 'none';
      checkoutForm.style.display = 'none';
      return;
    }

    cartEmptyMessage.style.display = 'none';
    cartItemsList.style.display = 'block';
    cartTotalSection.style.display = 'flex';
    checkoutForm.style.display = 'block';

    let total = 0;
    cartItemsList.innerHTML = cart.map(item => {
      const subtotal = item.price * item.quantity;
      total += subtotal;
      return `
        <div class="cart-item">
          <div>
            <div class="cart-item-name">${item.name}</div>
            <div class="cart-item-qty">Qty: ${item.quantity}</div>
          </div>
          <div style="display: flex; align-items: center; gap: 0.75rem;">
            <div class="cart-item-price">$${(subtotal / 100).toFixed(2)}</div>
            <button class="btn btn-secondary btn-danger" style="padding: 0.15rem 0.4rem; font-size: 0.75rem;" onclick="removeFromCart('${item.id}')">✕</button>
          </div>
        </div>
      `;
    }).join('');

    cartTotalAmount.innerText = `$${(total / 100).toFixed(2)}`;
  }

  // Handle Checkout Form Submission
  checkoutForm.addEventListener('submit', async (e) => {
    e.preventDefault();

    const customerName = document.getElementById('customer-name').value;
    const shippingAddress = document.getElementById('shipping-address').value;

    checkoutBtn.disabled = true;
    checkoutBtn.innerText = 'Processing Payment & Order...';

    try {
      const response = await fetch('/api/orders', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json'
        },
        body: JSON.stringify({
          items: cart.map(i => ({ id: itemToId(i.id), quantity: i.quantity })),
          customerName,
          shippingAddress
        })
      });

      const data = await response.json();

      if (!response.ok) {
        throw new Error(data.error || 'Server error. Checkout aborted.');
      }

      showToast(`Order created! ID: ${data.orderId}`, 'success');
      
      // Reset checkout form and cart
      cart = [];
      saveCart();
      updateCartUI();
    } catch (err) {
      console.error('Checkout failed:', err);
      showToast(err.message, 'error');
    } finally {
      checkoutBtn.disabled = false;
      checkoutBtn.innerText = 'Place Order (Simulate Checkout)';
    }
  });

  // Small safe mapper to clean ID matching
  function itemToId(id) {
    return String(id).trim();
  }

  // UI Toast alert handler
  function showToast(message, type = 'success') {
    const toast = document.getElementById('toast');
    const toastMsg = document.getElementById('toast-message');
    
    toastMsg.innerText = message;
    toast.className = 'toast show';
    
    if (type === 'success') {
      toast.classList.add('success');
      document.querySelector('.toast-icon').innerText = '✓';
    } else {
      toast.classList.add('error');
      document.querySelector('.toast-icon').innerText = '✕';
    }

    setTimeout(() => {
      toast.className = 'toast';
    }, 4000);
  }
});
