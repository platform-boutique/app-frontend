// Checkout form: the State field exists only while the country is the United States.
// For any other country it is hidden and disabled (so it is neither required nor submitted), and the country
// field takes the full row. Without JS the field stays visible and the server ignores a state sent for another country.
(function () {
  var country = document.getElementById('country');
  var field = document.getElementById('state-field');
  var state = document.getElementById('state');
  if (!country || !field || !state) { return; }
  var countryField = country.closest('.field');
  var us = country.getAttribute('data-us') || 'United States';

  function sync() {
    var isUS = country.value === us;
    field.hidden = !isUS;
    state.disabled = !isUS;
    state.required = isUS;
    if (countryField) {
      countryField.classList.toggle('span-4', isUS);
      countryField.classList.toggle('span-6', !isUS);
    }
  }
  country.addEventListener('change', sync);
  sync();
})();
